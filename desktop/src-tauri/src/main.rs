// Ender for the desktop: runs the photo-backup server (bundled as a "sidecar") and shows its
// settings page in a window, with a tray icon. Closing the window keeps Ender running in
// the tray so phones can keep backing up; "Quit Ender" stops the server too. If a server is
// already running (started some other way), the window just shows that one.
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::sync::Mutex;

use tauri::menu::{CheckMenuItem, Menu, MenuItem, PredefinedMenuItem};
use tauri::tray::{MouseButton, MouseButtonState, TrayIconBuilder, TrayIconEvent};
use tauri::{AppHandle, Manager, RunEvent, WindowEvent};
use tauri_plugin_autostart::{MacosLauncher, ManagerExt};
use tauri_plugin_shell::process::{CommandChild, CommandEvent};
use tauri_plugin_shell::ShellExt;

/// The language of the tray menu and the "Starting…" page: the one chosen in Ender Backup's
/// settings (the server's config.json), else the computer's. "es" or "en", like the server.
fn language() -> &'static str {
    let chosen = config_language();
    let tag = if chosen.is_empty() { sys_locale::get_locale().unwrap_or_default() } else { chosen };
    if tag.to_lowercase().starts_with("es") { "es" } else { "en" }
}

/// The "language" setting in the server's config.json (where Go's os.UserConfigDir puts it).
fn config_language() -> String {
    let dir = if cfg!(windows) {
        std::env::var("APPDATA").unwrap_or_default()
    } else if cfg!(target_os = "macos") {
        format!("{}/Library/Application Support", std::env::var("HOME").unwrap_or_default())
    } else {
        std::env::var("XDG_CONFIG_HOME").unwrap_or_else(|_| format!("{}/.config", std::env::var("HOME").unwrap_or_default()))
    };
    let path = std::env::var("PHOTO_BACKUP_CONFIG").unwrap_or_else(|_| format!("{dir}/photo-backup/config.json"));
    std::fs::read_to_string(path)
        .ok()
        .and_then(|s| serde_json::from_str::<serde_json::Value>(&s).ok())
        .and_then(|v| v.get("language").and_then(|l| l.as_str()).map(String::from))
        .unwrap_or_default()
}

/// The app's own text in `lang` (the settings page and server translate theirs).
fn tr(lang: &str, en: &'static str) -> &'static str {
    if lang != "es" {
        return en;
    }
    match en {
        "Open Ender" => "Abrir Ender",
        "Start at login" => "Abrir al iniciar sesión",
        "Quit Ender" => "Salir de Ender",
        "Photo & video backup" => "Respaldo de fotos y videos",
        "Starting…" => "Iniciando…",
        "The backup server stopped:" => "El servidor de respaldo se detuvo:",
        "Couldn't start the backup server:" => "No se pudo iniciar el servidor de respaldo:",
        _ => en,
    }
}

/// The server process this app started (None if one was already running).
struct Server(Mutex<Option<CommandChild>>);

fn show(app: &AppHandle) {
    if let Some(w) = app.get_webview_window("main") {
        let _ = w.unminimize();
        let _ = w.show();
        let _ = w.set_focus();
    }
}

/// Shows a message on the "Starting…" page (e.g. why the server didn't start).
fn say(app: &AppHandle, text: &str, error: bool) {
    if let Some(w) = app.get_webview_window("main") {
        let js = format!(
            "(() => {{ const m = document.getElementById('msg'); if (!m) return; m.textContent = {}; m.className = {}; \
             const s = document.getElementById('spin'); if (s && {}) s.style.display = 'none'; }})()",
            serde_json_string(text),
            if error { "'err'" } else { "''" },
            error
        );
        let _ = w.eval(&js);
    }
}

/// The "Starting…" page in the app's language (it may still be loading, so the script waits).
fn translate_starting_page(app: &AppHandle, lang: &str) {
    if lang == "en" {
        return;
    }
    if let Some(w) = app.get_webview_window("main") {
        let js = format!(
            "(function f() {{ const m = document.getElementById('msg'), s = document.querySelector('.sub');              if (!m || !s) return setTimeout(f, 30); document.documentElement.lang = {}; s.textContent = {};              if (m.className !== 'err') m.textContent = {}; }})()",
            serde_json_string(lang),
            serde_json_string(tr(lang, "Photo & video backup")),
            serde_json_string(tr(lang, "Starting…"))
        );
        let _ = w.eval(&js);
    }
}

fn serde_json_string(s: &str) -> String {
    let mut out = String::from("\"");
    for c in s.chars() {
        match c {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            c if (c as u32) < 0x20 => out.push_str(&format!("\\u{:04x}", c as u32)),
            c => out.push(c),
        }
    }
    out.push('"');
    out
}

fn stop_server(app: &AppHandle) {
    if let Some(child) = app.state::<Server>().0.lock().unwrap().take() {
        let _ = child.kill();
    }
}

fn start_server(app: &AppHandle) -> Result<(), Box<dyn std::error::Error>> {
    let (mut events, child) = app.shell().sidecar("photo-backup")?.args(["serve", "-desktop"]).spawn()?;
    app.state::<Server>().0.lock().unwrap().replace(child);
    let app = app.clone();
    tauri::async_runtime::spawn(async move {
        let mut opened = false;
        let mut last_error = String::new();
        while let Some(event) = events.recv().await {
            match event {
                CommandEvent::Stdout(bytes) => {
                    for line in String::from_utf8_lossy(&bytes).lines() {
                        if let Some(url) = line.trim().strip_prefix("PHOTO_BACKUP_URL=") {
                            if let (Some(w), Ok(url)) = (app.get_webview_window("main"), url.parse()) {
                                let _ = w.navigate(url);
                                opened = true;
                            }
                        }
                    }
                }
                CommandEvent::Stderr(bytes) => {
                    let text = String::from_utf8_lossy(&bytes).trim().to_string();
                    if !text.is_empty() {
                        last_error = text;
                    }
                }
                CommandEvent::Terminated(status) => {
                    // Exiting is expected when another server was already running (it printed
                    // its URL); otherwise say why it stopped.
                    app.state::<Server>().0.lock().unwrap().take();
                    if !opened || status.code.unwrap_or(0) != 0 {
                        let why = if last_error.is_empty() { format!("exit code {:?}", status.code) } else { last_error.clone() };
                        say(&app, &format!("{} {why}", tr(language(), "The backup server stopped:")), true);
                        show(&app);
                    }
                }
                _ => {}
            }
        }
    });
    Ok(())
}

fn main() {
    tauri::Builder::default()
        // A second launch (e.g. from the Start menu) just brings the window back.
        .plugin(tauri_plugin_single_instance::init(|app, _args, _cwd| show(app)))
        .plugin(tauri_plugin_shell::init())
        // At login Ender starts in the tray, without the window.
        .plugin(tauri_plugin_autostart::init(MacosLauncher::LaunchAgent, Some(vec!["--hidden"])))
        .manage(Server(Mutex::new(None)))
        .setup(|app| {
            let handle = app.handle();
            let lang = language();
            let open = MenuItem::with_id(app, "open", tr(lang, "Open Ender"), true, None::<&str>)?;
            let at_login = app.autolaunch().is_enabled().unwrap_or(false);
            let login = CheckMenuItem::with_id(app, "login", tr(lang, "Start at login"), true, at_login, None::<&str>)?;
            let quit = MenuItem::with_id(app, "quit", tr(lang, "Quit Ender"), true, None::<&str>)?;
            let separator = PredefinedMenuItem::separator(app)?;
            let menu = Menu::with_items(app, &[&open, &login, &separator, &quit])?;
            let login_item = login.clone();
            TrayIconBuilder::with_id("tray")
                .icon(app.default_window_icon().cloned().expect("app icon"))
                .tooltip("Ender Backup")
                .menu(&menu)
                .show_menu_on_left_click(false)
                .on_menu_event(move |app, event| match event.id.as_ref() {
                    "open" => show(app),
                    "login" => {
                        let launcher = app.autolaunch();
                        let _ = if launcher.is_enabled().unwrap_or(false) { launcher.disable() } else { launcher.enable() };
                        let _ = login_item.set_checked(launcher.is_enabled().unwrap_or(false));
                    }
                    "quit" => {
                        stop_server(app);
                        app.exit(0);
                    }
                    _ => {}
                })
                .on_tray_icon_event(|tray, event| {
                    if let TrayIconEvent::Click { button: MouseButton::Left, button_state: MouseButtonState::Up, .. } = event {
                        show(tray.app_handle());
                    }
                })
                .build(app)?;

            translate_starting_page(handle, lang);
            if let Err(e) = start_server(handle) {
                say(handle, &format!("{} {e}", tr(lang, "Couldn't start the backup server:")), true);
            }
            if !std::env::args().any(|a| a == "--hidden") {
                show(handle);
            }
            Ok(())
        })
        .on_window_event(|window, event| {
            // Closing the window keeps Ender (and backups) running in the tray.
            if let WindowEvent::CloseRequested { api, .. } = event {
                api.prevent_close();
                let _ = window.hide();
            }
        })
        .build(tauri::generate_context!())
        .expect("error while building Ender")
        .run(|app, event| {
            if let RunEvent::Exit = event {
                stop_server(app);
            }
        });
}
