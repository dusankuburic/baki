use tauri_plugin_shell::ShellExt;
use tauri_plugin_shell::process::CommandEvent;
use serde::{Serialize, Deserialize};
use tauri::{Emitter, Manager, State};
use tauri::menu::{Menu, MenuItem, Submenu, PredefinedMenuItem};
use tokio::sync::Mutex;

#[derive(Debug, Serialize, Deserialize, Clone, Default)]
struct BackendConfig {
    port: u16,
    token: String,
}

struct AppState {
    config: Mutex<Option<BackendConfig>>,
}

#[tauri::command]
async fn get_backend_config(state: State<'_, AppState>) -> Result<BackendConfig, String> {
    let config = state.config.lock().await;
    config.clone().ok_or_else(|| "Backend not ready".to_string())
}

#[cfg_attr(mobile, tauri::mobile_entry_point)]
pub fn run() {
    tauri::Builder::default()
        .plugin(tauri_plugin_shell::init())
        .plugin(tauri_plugin_dialog::init())
        .plugin(tauri_plugin_fs::init())
        .plugin(tauri_plugin_window_state::Builder::default().build())
        .plugin(tauri_plugin_single_instance::init(|app, args, _cwd| {
            let _ = app.emit("open-file", args);
        }))
        .plugin(tauri_plugin_log::Builder::default().build())
        .manage(AppState {
            config: Mutex::new(None),
        })
        .invoke_handler(tauri::generate_handler![get_backend_config])
        .setup(|app| {
            let handle = app.handle().clone();

            // Build native menu
            let file_menu = Submenu::with_id(&handle, "file", "File", true)?;
            let edit_menu = Submenu::with_id(&handle, "edit", "Edit", true)?;
            let view_menu = Submenu::with_id(&handle, "view", "View", true)?;
            let window_menu = Submenu::with_id(&handle, "window", "Window", true)?;
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
                &window_menu,
                &help_menu,
            ])?;

            handle.set_menu(menu)?;

            handle.on_menu_event(move |app, event| {
                let id = event.id().as_ref();
                let _ = app.emit("menu-event", id);
            });

            // Check for file arguments on startup
            let args: Vec<String> = std::env::args().collect();
            if args.len() > 1 {
                let handle_clone = handle.clone();
                let args_clone = args.clone();
                tauri::async_runtime::spawn(async move {
                    // Wait a bit for frontend to be ready
                    tokio::time::sleep(tokio::time::Duration::from_millis(1000)).await;
                    let _ = handle_clone.emit("open-file", args_clone);
                });
            }

            let handle_sidecar = handle.clone();
            tauri::async_runtime::spawn(async move {
                let (mut rx, _child) = handle_sidecar.shell()
                    .sidecar("pad-backend")
                    .expect("failed to create sidecar")
                    .spawn()
                    .expect("failed to spawn sidecar");

                while let Some(event) = rx.recv().await {
                    match event {
                        CommandEvent::Stdout(line) => {
                            let line_str = String::from_utf8_lossy(&line);
                            println!("Backend STDOUT: {}", line_str);
                            if let Ok(config) = serde_json::from_str::<BackendConfig>(&line_str) {
                                let state = handle_sidecar.state::<AppState>();
                                let mut config_lock = state.config.lock().await;
                                *config_lock = Some(config.clone());

                                // Emit event to frontend that backend is ready
                                handle_sidecar.emit("backend-ready", config).unwrap();
                                // Note: We don't break here anymore so we can keep logging
                            }
                        }
                        CommandEvent::Stderr(line) => {
                            let line_str = String::from_utf8_lossy(&line);
                            eprintln!("Backend STDERR: {}", line_str);
                        }
                        CommandEvent::Error(err) => {
                            eprintln!("Backend ERROR: {}", err);
                        }
                        CommandEvent::Terminated(payload) => {
                            eprintln!("Backend TERMINATED: {:?}", payload);
                        }
                        _ => {}
                    }
                }
            });

            Ok(())
        })
        .run(tauri::generate_context!())
        .expect("error while running tauri application");
}
