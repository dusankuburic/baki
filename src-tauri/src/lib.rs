use tauri_plugin_shell::ShellExt;
use tauri_plugin_shell::process::{CommandEvent, CommandChild};
use serde::{Serialize, Deserialize};
use tauri::{Emitter, Manager, State};
use tauri::menu::{Menu, MenuItem, PredefinedMenuItem, Submenu};
use tokio::sync::Mutex;
use std::sync::Arc;
use log::{info, warn, error};

const BACKEND_CONFIG_MARKER: &str = "CONFIG:";
const BACKEND_STARTUP_TIMEOUT_SECS: u64 = 30;
const BACKEND_MAX_RESTART_ATTEMPTS: u32 = 3;
const BACKEND_RESTART_DELAY_SECS: u64 = 2;

#[derive(Debug, Serialize, Deserialize, Clone, Default)]
struct BackendConfig {
    port: u16,
    token: String,
}

struct AppState {
    config: Mutex<Option<BackendConfig>>,
    child: Mutex<Option<CommandChild>>,
    // Set by restart_backend before killing the child so the sidecar event
    // loop doesn't also auto-respawn the intentionally terminated process.
    manual_restart: std::sync::atomic::AtomicBool,
}

fn filter_file_args(args: &[String]) -> Vec<String> {
    args.iter()
        .filter(|a| {
            let lower = a.to_lowercase();
            lower.ends_with(".txt") || lower.ends_with(".pad")
        })
        .cloned()
        .collect()
}

#[tauri::command]
async fn get_backend_config(state: State<'_, AppState>) -> Result<BackendConfig, String> {
    let config = state.config.lock().await;
    config.clone().ok_or_else(|| "Backend not ready".to_string())
}

#[tauri::command]
async fn restart_backend(handle: tauri::AppHandle) -> Result<(), String> {
    let state = handle.state::<AppState>();
    {
        let mut child_guard = state.child.lock().await;
        if let Some(child) = child_guard.take() {
            state
                .manual_restart
                .store(true, std::sync::atomic::Ordering::SeqCst);
            let _ = child.kill();
        }
    }
    spawn_sidecar(&handle);
    Ok(())
}

fn spawn_sidecar(handle: &tauri::AppHandle) {
    spawn_sidecar_attempt(handle, 0);
}

// attempt counts consecutive automatic restarts after crashes; a successful
// start (config received) resets the chain.
fn spawn_sidecar_attempt(handle: &tauri::AppHandle, attempt: u32) {
    let handle_clone = handle.clone();
    tauri::async_runtime::spawn(async move {
        let sidecar_cmd = match handle_clone.shell().sidecar("pad-backend") {
            Ok(cmd) => cmd,
            Err(e) => {
                error!("Failed to create sidecar: {}", e);
                let _ = handle_clone.emit("backend-error", format!("Failed to create sidecar: {}", e));
                return;
            }
        };

        let (mut rx, child) = match sidecar_cmd.spawn() {
            Ok(pair) => pair,
            Err(e) => {
                error!("Failed to spawn sidecar: {}", e);
                let _ = handle_clone.emit("backend-error", format!("Failed to spawn sidecar: {}", e));
                return;
            }
        };

        {
            let state = handle_clone.state::<AppState>();
            let mut child_guard = state.child.lock().await;
            *child_guard = Some(child);
        }

        let config_received = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let config_received_clone = config_received.clone();

        let timeout_handle = handle_clone.clone();
        let timeout_guard = tauri::async_runtime::spawn(async move {
            tokio::time::sleep(tokio::time::Duration::from_secs(BACKEND_STARTUP_TIMEOUT_SECS)).await;
            if !config_received_clone.load(std::sync::atomic::Ordering::Relaxed) {
                warn!("Backend did not send config within {}s", BACKEND_STARTUP_TIMEOUT_SECS);
                let _ = timeout_handle.emit("backend-error",
                    format!("Backend startup timed out after {}s", BACKEND_STARTUP_TIMEOUT_SECS));
            }
        });

        while let Some(event) = rx.recv().await {
            match event {
                CommandEvent::Stdout(line) => {
                    let line_str = String::from_utf8_lossy(&line);
                    info!("Backend STDOUT: {}", line_str);
                    if let Some(json_str) = line_str.strip_prefix(BACKEND_CONFIG_MARKER) {
                        match serde_json::from_str::<BackendConfig>(json_str.trim()) {
                            Ok(config) => {
                                config_received.store(true, std::sync::atomic::Ordering::Relaxed);
                                let state = handle_clone.state::<AppState>();
                                let mut config_lock = state.config.lock().await;
                                *config_lock = Some(config.clone());
                                if let Err(e) = handle_clone.emit("backend-ready", config) {
                                    warn!("Failed to emit backend-ready: {}", e);
                                }
                            }
                            Err(e) => {
                                warn!("Failed to parse backend config: {}", e);
                            }
                        }
                    }
                }
                CommandEvent::Stderr(line) => {
                    let line_str = String::from_utf8_lossy(&line);
                    info!("Backend STDERR: {}", line_str);
                }
                CommandEvent::Error(err) => {
                    error!("Backend ERROR: {}", err);
                }
                CommandEvent::Terminated(payload) => {
                    warn!("Backend TERMINATED: {:?}", payload);
                    let manual = {
                        let state = handle_clone.state::<AppState>();
                        let mut child_guard = state.child.lock().await;
                        *child_guard = None;
                        state
                            .manual_restart
                            .swap(false, std::sync::atomic::Ordering::SeqCst)
                    };
                    if let Err(e) = handle_clone.emit("backend-terminated", format!("{:?}", payload)) {
                        warn!("Failed to emit backend-terminated: {}", e);
                    }
                    if !manual {
                        // A crash after a successful start opens a fresh
                        // attempt chain; repeated startup failures keep
                        // counting toward the cap.
                        let next_attempt = if config_received.load(std::sync::atomic::Ordering::Relaxed) {
                            1
                        } else {
                            attempt + 1
                        };
                        if next_attempt <= BACKEND_MAX_RESTART_ATTEMPTS {
                            warn!(
                                "Restarting backend in {}s (attempt {}/{})",
                                BACKEND_RESTART_DELAY_SECS, next_attempt, BACKEND_MAX_RESTART_ATTEMPTS
                            );
                            tokio::time::sleep(tokio::time::Duration::from_secs(BACKEND_RESTART_DELAY_SECS)).await;
                            spawn_sidecar_attempt(&handle_clone, next_attempt);
                        } else {
                            error!(
                                "Backend crashed {} times in a row; giving up on auto-restart",
                                BACKEND_MAX_RESTART_ATTEMPTS
                            );
                            let _ = handle_clone.emit(
                                "backend-error",
                                "Backend crashed repeatedly; please restart the application".to_string(),
                            );
                        }
                    }
                    break;
                }
                _ => {}
            }
        }

        timeout_guard.abort();
    });
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_window_state::Builder::default().build())
        .plugin(tauri_plugin_single_instance::init(|app, args, _cwd| {
            let file_args = filter_file_args(&args);
            if !file_args.is_empty() {
                let _ = app.emit("open-file", file_args);
            }
        }))
        .plugin(tauri_plugin_log::Builder::default().build())
        .manage(AppState {
            config: Mutex::new(None),
            child: Mutex::new(None),
            manual_restart: std::sync::atomic::AtomicBool::new(false),
        })
        .invoke_handler(tauri::generate_handler![get_backend_config, restart_backend])
        .setup(|app| {
            let handle = app.handle().clone();

            let file_menu = Submenu::with_id(&handle, "file", "File", true)?;
            let edit_menu = Submenu::with_id(&handle, "edit", "Edit", true)?;
            let view_menu = Submenu::with_id(&handle, "view", "View", true)?;
            let help_menu = Submenu::with_id(&handle, "help", "Help", true)?;

            file_menu.append_items(&[
                &MenuItem::with_id(&handle, "file.open", "Open File...", true, Some("CmdOrCtrl+O"))?,
                &MenuItem::with_id(&handle, "file.open.folder", "Open Folder...", true, Some("CmdOrCtrl+Shift+O"))?,
                &PredefinedMenuItem::separator(&handle)?,
                &MenuItem::with_id(&handle, "file.export.pdf", "Export as PDF...", true, Some("CmdOrCtrl+E"))?,
                &MenuItem::with_id(&handle, "file.export.md", "Export as Markdown...", true, Some("CmdOrCtrl+Shift+E"))?,
                &PredefinedMenuItem::separator(&handle)?,
                &MenuItem::with_id(&handle, "file.close.tab", "Close Tab", true, Some("CmdOrCtrl+W"))?,
                &PredefinedMenuItem::separator(&handle)?,
                &PredefinedMenuItem::quit(&handle, None)?,
            ])?;

            edit_menu.append_items(&[
                &PredefinedMenuItem::undo(&handle, None)?,
                &PredefinedMenuItem::redo(&handle, None)?,
                &PredefinedMenuItem::separator(&handle)?,
                &PredefinedMenuItem::cut(&handle, None)?,
                &PredefinedMenuItem::copy(&handle, None)?,
                &PredefinedMenuItem::paste(&handle, None)?,
                &PredefinedMenuItem::select_all(&handle, None)?,
            ])?;

            view_menu.append_items(&[
                &MenuItem::with_id(&handle, "view.toggle.sidebar", "Toggle Sidebar", true, Some("CmdOrCtrl+B"))?,
                &MenuItem::with_id(&handle, "view.toggle.inspector", "Toggle Inspector", true, Some("CmdOrCtrl+I"))?,
                &MenuItem::with_id(&handle, "view.toggle.mode", "Toggle View Mode", true, Some("CmdOrCtrl+G"))?,
                &PredefinedMenuItem::separator(&handle)?,
                &MenuItem::with_id(&handle, "view.theme.toggle", "Toggle Theme", true, Some("CmdOrCtrl+Shift+T"))?,
                &PredefinedMenuItem::separator(&handle)?,
                &MenuItem::with_id(&handle, "window.reload", "Reload", true, Some("CmdOrCtrl+R"))?,
                &PredefinedMenuItem::fullscreen(&handle, None)?,
            ])?;

            help_menu.append_items(&[
                &MenuItem::with_id(&handle, "help.shortcuts", "Keyboard Shortcuts", true, Some("?"))?,
                &PredefinedMenuItem::about(&handle, None, None)?,
            ])?;

            let menu = Menu::with_items(&handle, &[
                &file_menu,
                &edit_menu,
                &view_menu,
                &help_menu,
            ])?;

            handle.set_menu(menu)?;

            handle.on_menu_event(move |app, event| {
                let id = event.id().as_ref();
                let _ = app.emit("menu-event", id);
            });

            let args: Vec<String> = std::env::args().collect();
            let file_args = filter_file_args(&args);
            if !file_args.is_empty() {
                let handle_clone = handle.clone();
                let fa_clone = file_args.clone();
                tauri::async_runtime::spawn(async move {
                    tokio::time::sleep(tokio::time::Duration::from_millis(1000)).await;
                    let _ = handle_clone.emit("open-file", fa_clone);
                });
            }

            spawn_sidecar(&handle);

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
