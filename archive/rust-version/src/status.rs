use std::io::Write;
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Instant;

use crate::platform::NetMonitor;

#[derive(Clone, Debug)]
pub struct IpInfo {
    pub ip: String,
    pub city: String,
    pub country: String,
}

fn http_get_json(url: &str) -> Result<serde_json::Value, String> {
    // 使用 no_proxy 环境变量，确保 curl 不使用代理，直接使用系统网络
    let cmd = format!("no_proxy=* curl -s -m 10 {}", url);
    let output = Command::new("sh")
        .args(["-c", &cmd])
        .output()
        .map_err(|e| format!("shell exec failed: {}", e))?;
    
    if !output.status.success() {
        return Err("curl request failed".to_string());
    }
    
    let json_str = String::from_utf8_lossy(&output.stdout);
    if json_str.is_empty() {
        return Err("curl returned empty response".to_string());
    }
    
    serde_json::from_str(&json_str).map_err(|_e| "json parse failed".to_string())
}

/// 通过代理查询 IP（用于 VPN 启动后查询 VPN 出口 IP）
pub fn fetch_ip_via_proxy() -> Result<IpInfo, String> {
    // 第一步：获取 IP 地址（通过 VPN）
    let ip_services = [
        "http://ifconfig.me/ip",
        "http://ipinfo.io/ip",
        "http://icanhazip.com",
    ];

    let mut ip = String::new();
    for service in &ip_services {
        let output = Command::new("sh")
            .args(["-c", &format!("curl -s -m 10 {}", service)])
            .output()
            .map_err(|e| format!("curl exec failed: {}", e))?;

        if output.status.success() {
            ip = String::from_utf8_lossy(&output.stdout).trim().to_string();
            if !ip.is_empty() {
                break;
            }
        }
    }

    if ip.is_empty() {
        return Err("all ip lookup services failed".to_string());
    }

    // 调用通用的 IP 地理位置查询函数
    fetch_ip_geolocation(&ip)
}

/// 根据 IP 地址查询其地理位置信息
pub fn fetch_ip_geolocation(ip: &str) -> Result<IpInfo, String> {
    // 用 IP 查询地区信息 - 使用 ipinfo.io 的免费 API
    let geo_services = [
        format!("http://ipinfo.io/{}/json", ip),
    ];

    for service in &geo_services {
        let output = Command::new("sh")
            .args(["-c", &format!("no_proxy=* curl -s -m 10 -k {}", service)])
            .output()
            .map_err(|e| format!("curl exec failed: {}", e))?;

        if output.status.success() {
            let json_str = String::from_utf8_lossy(&output.stdout);
            if let Ok(json) = serde_json::from_str::<serde_json::Value>(&json_str) {
                let city = json.get("city")
                    .and_then(|v| v.as_str())
                    .or_else(|| json.get("regionName").and_then(|v| v.as_str()))
                    .or_else(|| json.get("region").and_then(|v| v.as_str()))
                    .unwrap_or("?")
                    .to_string();
                let country = json.get("countryCode")
                    .and_then(|v| v.as_str())
                    .or_else(|| json.get("country").and_then(|v| v.as_str()))
                    .unwrap_or("?")
                    .to_string();
                return Ok(IpInfo { ip: ip.to_string(), city, country });
            }
        }
    }

    // 如果地区查询失败，返回 IP 和默认地区
    Ok(IpInfo {
        ip: ip.to_string(),
        city: "?".to_string(),
        country: "?".to_string(),
    })
}

/// 绕过代理查询 IP（用于 VPN 启动前查询本地出口 IP）
pub fn fetch_ip() -> Result<IpInfo, String> {
    // 第一步：获取 IP 地址
    let ip_services = [
        "https://ifconfig.me/ip",
        "https://ipinfo.io/ip",
        "https://icanhazip.com",
    ];

    let mut ip = String::new();
    for service in &ip_services {
        let output = Command::new("sh")
            .args(["-c", &format!("no_proxy=* curl -s -m 5 {}", service)])
            .output()
            .map_err(|e| format!("curl exec failed: {}", e))?;

        if output.status.success() {
            ip = String::from_utf8_lossy(&output.stdout).trim().to_string();
            if !ip.is_empty() {
                break;
            }
        }
    }

    if ip.is_empty() {
        return Err("all ip lookup services failed".to_string());
    }

    // 第二步：用 IP 查询地区信息
    let geo_services = [
        format!("https://ipinfo.io/{}/json", ip),
        format!("http://ip-api.com/json/{}", ip),
    ];

    for service in &geo_services {
        let output = Command::new("sh")
            .args(["-c", &format!("no_proxy=* curl -s -m 5 {}", service)])
            .output()
            .map_err(|e| format!("curl exec failed: {}", e))?;

        if output.status.success() {
            let json_str = String::from_utf8_lossy(&output.stdout);
            if let Ok(json) = serde_json::from_str::<serde_json::Value>(&json_str) {
                let city = json.get("city")
                    .and_then(|v| v.as_str())
                    .or_else(|| json.get("region").and_then(|v| v.as_str()))
                    .unwrap_or("?")
                    .to_string();
                let country = json.get("country")
                    .and_then(|v| v.as_str())
                    .unwrap_or("?")
                    .to_string();
                return Ok(IpInfo { ip, city, country });
            }
        }
    }

    // 如果地区查询失败，返回 IP 和默认地区
    Ok(IpInfo {
        ip,
        city: "?".to_string(),
        country: "?".to_string(),
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


/// Wait for sing-box to be ready by probing the clash API and checking logs for fatal errors.
fn wait_for_singbox_ready(stop: &AtomicBool) -> bool {
    let log_path = crate::runner::log_path();
    for _ in 0..30 {
        std::thread::sleep(std::time::Duration::from_secs(1));
        if stop.load(Ordering::Relaxed) || crate::runner::SHUTTING_DOWN.load(Ordering::Relaxed) {
            return false;
        }
        // Check for fatal errors in log (read tail only to avoid blocking on large logs)
        if let Ok(file) = std::fs::File::open(&log_path) {
            use std::io::{Read, Seek, SeekFrom};
            let mut file = file;
            let len = file.metadata().map(|m| m.len()).unwrap_or(0);
            // Only read last 4KB — FATAL errors appear near the end
            if len > 4096 {
                let _ = file.seek(SeekFrom::End(-4096));
            }
            let mut tail = String::new();
            if file.read_to_string(&mut tail).is_ok() && tail.contains("FATAL") {
                for line in tail.lines() {
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
        // Clash API responding means sing-box is up
        if std::net::TcpStream::connect_timeout(
            &"127.0.0.1:19090".parse().unwrap(),
            std::time::Duration::from_millis(200),
        )
        .is_ok()
        {
            return true;
        }
    }
    false
}

/// Background thread: wait for TUN, print IP, then show live speed via NetMonitor.
/// Respects `stop` signal for clean thread shutdown on failover.
pub fn start_live_monitor(stop: Arc<AtomicBool>, pre_fetched_ip: Option<IpInfo>) {
    std::thread::spawn(move || {
        if !wait_for_singbox_ready(&stop) {
            if !stop.load(Ordering::Relaxed)
                && !crate::runner::SHUTTING_DOWN.load(Ordering::Relaxed)
            {
                eprintln!("\r\x1b[K  \x1b[2mstatus\x1b[0m  \x1b[31mfailed\x1b[0m");
            }
            return;
        }

        // Wait a bit for TUN routing to stabilise before querying IP
        std::thread::sleep(std::time::Duration::from_secs(3));

        // 优先使用预查询的服务器 IP，否则通过 VPN 查询出口 IP
        let ip_info = if let Some(info) = pre_fetched_ip {
            info
        } else {
            match fetch_ip_via_proxy() {
                Ok(info) => info,
                Err(_) => IpInfo {
                    ip: "?".to_string(),
                    city: "?".to_string(),
                    country: "?".to_string(),
                },
            }
        };

        let ip_str = format!(
            "\x1b[32m●\x1b[0m {} ({}, {})",
            ip_info.ip, ip_info.city, ip_info.country
        );
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
