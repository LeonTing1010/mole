use std::io::BufRead;
use std::process::{Command, Stdio};

/// Operating system name for sing-box release tarballs.
pub fn os_name() -> &'static str {
    if cfg!(target_os = "macos") {
        "darwin"
    } else {
        "linux"
    }
}

/// CPU architecture for sing-box release tarballs.
pub fn arch_name() -> &'static str {
    if cfg!(target_arch = "aarch64") {
        "arm64"
    } else {
        "amd64"
    }
}

/// Detect the primary network interface.
pub fn default_interface() -> String {
    #[cfg(target_os = "macos")]
    {
        return "en0".to_string();
    }

    #[cfg(target_os = "linux")]
    {
        if let Ok(output) = Command::new("ip")
            .args(["route", "show", "default"])
            .output()
        {
            if output.status.success() {
                let text = String::from_utf8_lossy(&output.stdout);
                if let Some(idx) = text.find("dev ") {
                    if let Some(iface) = text[idx + 4..].split_whitespace().next() {
                        return iface.to_string();
                    }
                }
            }
        }
        return "eth0".to_string();
    }

    #[allow(unreachable_code)]
    "eth0".to_string()
}

/// Detect the local LAN IP address.
pub fn local_ip() -> Option<String> {
    // Connect a UDP socket to a public address to determine local IP
    // (no actual traffic is sent)
    let sock = std::net::UdpSocket::bind("0.0.0.0:0").ok()?;
    sock.connect("8.8.8.8:80").ok()?;
    let addr = sock.local_addr().ok()?;
    Some(addr.ip().to_string())
}

// ── Cross-platform network speed monitor ────────────────────────────

#[cfg(target_os = "macos")]
pub use self::macos_monitor::NetMonitor;
#[cfg(target_os = "linux")]
pub use self::linux_monitor::NetMonitor;
#[cfg(not(any(target_os = "macos", target_os = "linux")))]
pub use self::fallback_monitor::NetMonitor;

#[cfg(target_os = "macos")]
mod macos_monitor {
    use super::*;
    use std::io::BufReader;

    pub struct NetMonitor {
        child: std::process::Child,
        reader: BufReader<std::process::ChildStdout>,
    }

    impl NetMonitor {
        pub fn new() -> Option<Self> {
            let iface = super::default_interface();
            let mut child = Command::new("netstat")
                .args(["-I", &iface, "-w", "1"])
                .stdout(Stdio::piped())
                .stderr(Stdio::null())
                .spawn()
                .ok()?;
            let stdout = child.stdout.take()?;
            Some(NetMonitor {
                child,
                reader: BufReader::new(stdout),
            })
        }

        /// Block until the next 1-second sample. Returns (rx_delta, tx_delta).
        pub fn next_sample(&mut self) -> Option<(u64, u64)> {
            loop {
                let mut line = String::new();
                match self.reader.read_line(&mut line) {
                    Ok(0) | Err(_) => return None,
                    Ok(_) => {}
                }
                let cols: Vec<&str> = line.split_whitespace().collect();
                if cols.len() < 7 {
                    continue;
                }
                match (cols[2].parse::<u64>(), cols[5].parse::<u64>()) {
                    (Ok(rx), Ok(tx)) => return Some((rx, tx)),
                    _ => continue,
                }
            }
        }
    }

    impl Drop for NetMonitor {
        fn drop(&mut self) {
            self.child.kill().ok();
            self.child.wait().ok();
        }
    }
}

#[cfg(target_os = "linux")]
mod linux_monitor {
    pub struct NetMonitor {
        interface: String,
        prev_rx: u64,
        prev_tx: u64,
    }

    impl NetMonitor {
        pub fn new() -> Option<Self> {
            let interface = super::default_interface();
            let (rx, tx) = read_proc_net_dev(&interface)?;
            Some(NetMonitor {
                interface,
                prev_rx: rx,
                prev_tx: tx,
            })
        }

        pub fn next_sample(&mut self) -> Option<(u64, u64)> {
            std::thread::sleep(std::time::Duration::from_secs(1));
            let (rx, tx) = read_proc_net_dev(&self.interface)?;
            let delta_rx = rx.saturating_sub(self.prev_rx);
            let delta_tx = tx.saturating_sub(self.prev_tx);
            self.prev_rx = rx;
            self.prev_tx = tx;
            Some((delta_rx, delta_tx))
        }
    }

    fn read_proc_net_dev(interface: &str) -> Option<(u64, u64)> {
        let content = std::fs::read_to_string("/proc/net/dev").ok()?;
        for line in content.lines() {
            let line = line.trim();
            if let Some(rest) = line.strip_prefix(interface) {
                if let Some(after_colon) = rest.strip_prefix(':') {
                    let cols: Vec<&str> = after_colon.split_whitespace().collect();
                    if cols.len() >= 9 {
                        let rx = cols[0].parse::<u64>().ok()?;
                        let tx = cols[8].parse::<u64>().ok()?;
                        return Some((rx, tx));
                    }
                }
            }
        }
        None
    }
}

#[cfg(not(any(target_os = "macos", target_os = "linux")))]
mod fallback_monitor {
    pub struct NetMonitor;
    impl NetMonitor {
        pub fn new() -> Option<Self> {
            None
        }
        pub fn next_sample(&mut self) -> Option<(u64, u64)> {
            None
        }
    }
}
