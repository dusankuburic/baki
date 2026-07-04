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
// How long to wait for the backend to exit after SIGTERM before falling back to
// a hard kill. The Go server's fx shutdown normally completes well within this;
// the fallback only triggers if it hangs. Mirrors the hard-kill-free grace the
// production-readiness review asked for, so an in-flight write isn't interrupted
// on app close or manual restart.
const BACKEND_SHUTDOWN_GRACE_SECS: u64 = 5;

#[derive(Debug, Serialize, Deserialize, Clone, Default)]
struct BackendConfig {
    port: u16,
    token: String,
}

// RawBackendConfig is the on-wire shape of the backend's `CONFIG:` stdout line.
// The backend no longer emits the signing secret in stdout (it lands in a 0600
// file under the config dir); it emits `tokenPath`, which we resolve to `token`
// by reading the file. `token` is still accepted for backward compatibility
// with older backend builds that emitted it directly.
#[derive(Debug, Deserialize)]
struct RawBackendConfig {
    port: u16,
    #[serde(default)]
    token: Option<String>,
    #[serde(default)]
    #[serde(rename = "tokenPath")]
    token_path: Option<String>,
}

impl RawBackendConfig {
    /// Resolve the signing secret: prefer reading it from tokenPath (the secure
    /// file handoff); fall back to a directly-emitted token for compatibility.
    fn resolve(self) -> Result<BackendConfig, String> {
        let token = if let Some(path) = self.token_path {
            std::fs::read_to_string(&path)
                .map_err(|e| format!("failed to read session key at {}: {}", path, e))?
                .trim()
                .to_string()
        } else if let Some(t) = self.token {
            t
        } else {
            return Err("backend config missing tokenPath and token".to_string());
        };
        Ok(BackendConfig {
            port: self.port,
            token,
        })
    }
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
    let child = {
        let mut child_guard = state.child.lock().await;
        child_guard.take()
    };
    if let Some(child) = child {
        // Mark an intentional termination so the sidecar event loop doesn't
        // auto-respawn the process we're about to stop.
        state
            .manual_restart
            .store(true, std::sync::atomic::Ordering::SeqCst);
        // Wait for the backend to actually exit before respawning so the new
        // instance can bind the port; graceful_terminate returns once the old
        // process is gone (gracefully or via the hard-kill fallback).
        graceful_terminate(child).await;
    }
    spawn_sidecar(&handle);
    Ok(())
}

// term_signal sends a graceful termination request to a process. On Unix this is
// SIGTERM, which the Go backend handles via fx to flush in-flight writes. On
// non-Unix there is no equivalent, so it is a no-op (the hard-kill fallback
// applies immediately).
#[cfg(unix)]
fn term_signal(pid: u32) {
    let _ = std::process::Command::new("kill")
        .arg("-TERM")
        .arg(pid.to_string())
        .spawn();
}
#[cfg(not(unix))]
fn term_signal(_pid: u32) {}

// process_alive reports whether the given pid is still running. Used to break
// out of the grace loop as soon as the backend exits, so a normal (fast) fx
// shutdown doesn't pay the full grace delay.
#[cfg(unix)]
fn process_alive(pid: u32) -> bool {
    std::process::Command::new("kill")
        .arg("-0")
        .arg(pid.to_string())
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}
#[cfg(not(unix))]
fn process_alive(_pid: u32) -> bool {
    false
}

// graceful_terminate sends SIGTERM, polls for exit up to the grace period, then
// hard-kills as a fallback (a no-op if the process already exited). The plugin's
// CommandChild::kill is a hard kill with no grace period, which can interrupt a
// write; this wraps it with a graceful window first.
async fn graceful_terminate(child: CommandChild) {
    let pid = child.pid();
    term_signal(pid);
    for _ in 0..BACKEND_SHUTDOWN_GRACE_SECS {
        tokio::time::sleep(tokio::time::Duration::from_secs(1)).await;
        if !process_alive(pid) {
            break;
        }
    }
    let _ = child.kill();
}

// graceful_terminate_blocking is the sync variant for the app-exit path (which
// runs outside an async context).
fn graceful_terminate_blocking(child: CommandChild) {
    let pid = child.pid();
    term_signal(pid);
    for _ in 0..BACKEND_SHUTDOWN_GRACE_SECS {
        std::thread::sleep(std::time::Duration::from_secs(1));
        if !process_alive(pid) {
            break;
        }
    }
    let _ = child.kill();
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
                    // The CONFIG line now carries only the port + a filesystem
                    // path to the secret (never the secret itself), but other
                    // stdout lines can still contain sensitive request data, so
                    // we avoid mirroring backend stdout into the app log at
                    // info level. Debug-level mirroring is intentional in the
                    // tracing build only.
                    if !line_str.starts_with(BACKEND_CONFIG_MARKER) {
                        info!("Backend STDOUT: {}", line_str);
                    } else {
                        info!("Backend STDOUT: <config line redacted>");
                    }
                    if let Some(json_str) = line_str.strip_prefix(BACKEND_CONFIG_MARKER) {
                        match serde_json::from_str::<RawBackendConfig>(json_str.trim()) {
                            Ok(raw) => match raw.resolve() {
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
                                    warn!("Failed to resolve backend session secret: {}", e);
                                }
                            },
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
    let app = tauri::Builder::default()
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
        .build(tauri::generate_context!())
        .expect("error while building tauri application");

    app.run(|app_handle, event| {
        // On exit, stop the backend sidecar gracefully (SIGTERM + grace, then
        // hard-kill fallback) so it can flush in-flight writes instead of being
        // orphaned or hard-killed. Without this the child would keep running
        // (holding its port) after the app closes.
        if let tauri::RunEvent::ExitRequested { .. } = event {
            let state = app_handle.state::<AppState>();
            let child = state.child.blocking_lock().take();
            if let Some(child) = child {
                state
                    .manual_restart
                    .store(true, std::sync::atomic::Ordering::SeqCst);
                graceful_terminate_blocking(child);
            }
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;

    // TestRawBackendConfig_ResolvesFromTokenPath is the regression test for the
    // secret-in-stdout fix: the backend now emits tokenPath (a path to a 0600
    // file) instead of the signing secret itself. The resolver must read the
    // secret from that file so the in-memory BackendConfig still carries the
    // token the frontend needs, without the secret ever appearing on stdout.
    #[test]
    fn test_raw_backend_config_resolves_from_token_path() {
        let dir = std::env::temp_dir();
        let path = dir.join(format!(
            "pad-session-key-{}-{}.key",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::write(&path, "  SECRET-FROM-FILE  \n").unwrap();

        let json = format!(
            r#"{{"port":12345,"tokenPath":"{}"}}"#,
            path.display().to_string().replace('\\', "\\\\")
        );
        let raw: RawBackendConfig = serde_json::from_str(&json).unwrap();
        let cfg = raw.resolve().expect("resolve should succeed");
        assert_eq!(cfg.port, 12345);
        // The resolver trims surrounding whitespace/newlines from the file.
        assert_eq!(cfg.token, "SECRET-FROM-FILE");

        let _ = std::fs::remove_file(&path);
    }

    // TestRawBackendConfig_FallsBackToDirectToken keeps the wire format backward
    // compatible: an older backend that still emits `token` directly is accepted.
    #[test]
    fn test_raw_backend_config_falls_back_to_direct_token() {
        let raw: RawBackendConfig =
            serde_json::from_str(r#"{"port":7,"token":"legacy-tok"}"#).unwrap();
        let cfg = raw.resolve().expect("resolve should succeed");
        assert_eq!(cfg.port, 7);
        assert_eq!(cfg.token, "legacy-tok");
    }

    // TestRawBackendConfig_MissingTokenErrors ensures we fail closed (rather
    // than silently producing an empty-token config) when neither tokenPath nor
    // token is present.
    #[test]
    fn test_raw_backend_config_missing_token_errors() {
        let raw: RawBackendConfig = serde_json::from_str(r#"{"port":9}"#).unwrap();
        let err = raw.resolve().unwrap_err();
        assert!(err.contains("missing"), "unexpected error: {}", err);
    }
}
