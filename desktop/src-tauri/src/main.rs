#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

#[tauri::command]
fn pairing_api_base() -> String {
    if let Some(url) = option_env!("TUNNELWAY_API_URL") {
        url.trim_end_matches('/').to_string()
    } else if cfg!(debug_assertions) {
        "http://localhost:8080".to_string()
    } else {
        String::new()
    }
}

fn main() {
    tauri::Builder::default()
        .invoke_handler(tauri::generate_handler![pairing_api_base])
        .run(tauri::generate_context!())
        .expect("failed to run Tunnelway");
}
