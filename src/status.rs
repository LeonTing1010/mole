use std::io::Write;
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Instant;

use crate::platform::NetMonitor;

pub struct IpInfo {
    pub ip: String,
    pub city: String,
    pub country: String,
    pub org: String,
}

pub fn fetch_ip() -> Result<IpInfo, String> {
    let resp = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .map_err(|e| e.to_string())?
        .get("https://ipinfo.io/json")
        .send()
        .map_err(|e| e.to_string())?;

    let json: serde_json::Value = resp.json().map_err(|e| e.to_string())?;
    Ok(IpInfo {
        ip: json["ip"].as_str().unwrap_or("?").to_string(),
        city: json["city"].as_str().unwrap_or("?").to_string(),
        country: json["country"].as_str().unwrap_or("?").to_string(),
        org: json["org"].as_str().unwrap_or("?").to_string(),
    })
}

pub fn measure_latency(host: &str) -> Option<u128> {
    let start = Instant::now();
    let output = Command::new("ping")
        .args(["-c", "1", "-W", "3", host])
        .output()
        .ok()?;
    if output.status.success() {
        Some(start.elapsed().as_millis())
    } else {
        None
    }
}

pub fn is_singbox_running() -> bool {
    Command::new("pgrep")
        .args(["-f", "sing-box run"])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false)
}

fn format_speed(bytes_per_sec: u64) -> String {
    if bytes_per_sec >= 1_048_576 {
        format!("{:.1} MB/s", bytes_per_sec as f64 / 1_048_576.0)
    } else if bytes_per_sec >= 1024 {
        format!("{:.0} KB/s", bytes_per_sec as f64 / 1024.0)
    } else {
        format!("{bytes_per_sec} B/s")
    }
}

fn format_bytes(bytes: u64) -> String {
    if bytes >= 1_073_741_824 {
        format!("{:.2} GB", bytes as f64 / 1_073_741_824.0)
    } else if bytes >= 1_048_576 {
        format!("{:.1} MB", bytes as f64 / 1_048_576.0)
    } else if bytes >= 1024 {
        format!("{:.0} KB", bytes as f64 / 1024.0)
    } else {
        format!("{bytes} B")
    }
}

pub fn print_status() {
    // Daemon info
    let pid_file = crate::runner::pid_path();
    if pid_file.exists() {
        if let Ok(pid) = std::fs::read_to_string(&pid_file) {
            println!("daemon: running (pid={})", pid.trim());
        }
    }

    if !is_singbox_running() {
        println!("status: disconnected");
        return;
    }

    println!("status: connected");

    match fetch_ip() {
        Ok(info) => {
            println!("    ip: {} ({}, {})", info.ip, info.city, info.country);
            println!("   org: {}", info.org);
        }
        Err(e) => println!("    ip: failed ({e})"),
    }

    match measure_latency("8.8.8.8") {
        Some(ms) => println!("  ping: {ms}ms (8.8.8.8)"),
        None => println!("  ping: timeout"),
    }
}

/// Wait for sing-box to report "started" in its log file.
fn wait_for_singbox_ready(stop: &AtomicBool) -> bool {
    let log_path = crate::runner::log_path();
    for _ in 0..30 {
        std::thread::sleep(std::time::Duration::from_secs(1));
        if stop.load(Ordering::Relaxed)
            || crate::runner::SHUTTING_DOWN.load(Ordering::Relaxed)
        {
            return false;
        }
        if let Ok(log) = std::fs::read_to_string(&log_path) {
            if log.contains("sing-box started") {
                return true;
            }
            if log.contains("FATAL") {
                for line in log.lines() {
                    if line.contains("FATAL") {
                        eprintln!(
                            "  \x1b[31merror:\x1b[0m {}",
                            line.split("FATAL").last().unwrap_or(line).trim()
                        );
                    }
                }
                return false;
            }
        }
    }
    false
}

/// Background thread: wait for TUN, print IP, then show live speed via NetMonitor.
/// Respects `stop` signal for clean thread shutdown on failover.
pub fn start_live_monitor(stop: Arc<AtomicBool>) {
    std::thread::spawn(move || {
        if !wait_for_singbox_ready(&stop) {
            if !stop.load(Ordering::Relaxed)
                && !crate::runner::SHUTTING_DOWN.load(Ordering::Relaxed)
            {
                eprintln!("\r\x1b[K  \x1b[2mstatus\x1b[0m  \x1b[31mfailed\x1b[0m");
            }
            return;
        }

        let ip_str = match fetch_ip() {
            Ok(info) => format!(
                "\x1b[32m●\x1b[0m {} ({}, {})",
                info.ip, info.city, info.country
            ),
            Err(_) => "\x1b[32m●\x1b[0m connected".to_string(),
        };

        eprintln!("\r\x1b[K  \x1b[2mstatus\x1b[0m  {ip_str}");
        eprintln!();

        let mut monitor = match NetMonitor::new() {
            Some(m) => m,
            None => {
                eprintln!("  speed: monitoring unavailable");
                return;
            }
        };

        let start = Instant::now();
        let mut total_down: u64 = 0;
        let mut total_up: u64 = 0;

        loop {
            if stop.load(Ordering::Relaxed)
                || crate::runner::SHUTTING_DOWN.load(Ordering::Relaxed)
                || !is_singbox_running()
            {
                eprint!("\r\x1b[K");
                break;
            }

            match monitor.next_sample() {
                Some((down_bytes, up_bytes)) => {
                    total_down += down_bytes;
                    total_up += up_bytes;

                    let elapsed = start.elapsed().as_secs();
                    let hours = elapsed / 3600;
                    let mins = (elapsed % 3600) / 60;
                    let secs = elapsed % 60;

                    eprint!(
                        "\r\x1b[K  \x1b[36m↓\x1b[0m {:>10} ({:>9})  \x1b[35m↑\x1b[0m {:>10} ({:>9})  \x1b[2m{:02}:{:02}:{:02}\x1b[0m",
                        format_speed(down_bytes),
                        format_bytes(total_down),
                        format_speed(up_bytes),
                        format_bytes(total_up),
                        hours, mins, secs
                    );
                    std::io::stderr().flush().ok();
                }
                None => break,
            }
        }
    });
}
