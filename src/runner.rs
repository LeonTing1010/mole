use crate::platform;

use std::fs;
use std::io::Write;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

#[cfg(unix)]
use std::os::unix::process::ExitStatusExt;

const SINGBOX_VERSION: &str = "1.13.7";
const MAX_RETRIES: u32 = 10;

/// Global stop signal — set by Ctrl+C or shutdown paths.
pub static SHUTTING_DOWN: AtomicBool = AtomicBool::new(false);

/// PID of the current sudo+sing-box process (0 = not running).
pub static SINGBOX_PID: AtomicU32 = AtomicU32::new(0);

/// Set by keepalive watchdog before killing sing-box for health reasons.
/// Checked by run_singbox to distinguish health-kill from user-kill.
static HEALTH_KILL: AtomicBool = AtomicBool::new(false);

/// Why `run_singbox` returned.
pub enum ExitReason {
    /// User-initiated stop (Ctrl+C, SIGTERM, clean exit).
    Clean,
    /// Crashed more times than MAX_RETRIES allows.
    MaxRetries,
}

// ── Paths ───────────────────────────────────────────────────────────

pub fn mole_dir() -> PathBuf {
    let dir = dirs::home_dir()
        .expect("cannot find home directory")
        .join(".mole");
    fs::create_dir_all(&dir).expect("cannot create ~/.mole");
    dir
}

fn bin_dir() -> PathBuf {
    let dir = mole_dir().join("bin");
    fs::create_dir_all(&dir).expect("cannot create ~/.mole/bin");
    dir
}

pub fn singbox_bin_path() -> PathBuf {
    bin_dir().join("sing-box")
}

pub fn config_path() -> PathBuf {
    mole_dir().join("config.json")
}

pub fn singbox_installed() -> bool {
    singbox_bin_path().exists()
}

pub fn log_path() -> PathBuf {
    mole_dir().join("sing-box.log")
}

pub fn pid_path() -> PathBuf {
    mole_dir().join("mole.pid")
}

pub fn prev_log_path() -> PathBuf {
    mole_dir().join("sing-box.prev.log")
}

// ── PID file locking ───────────────────────────────────────────────

/// Check if another mole instance is already running. Returns Err with PID if so.
pub fn check_already_running() -> Result<(), String> {
    let path = pid_path();
    if !path.exists() {
        return Ok(());
    }
    let pid_str = fs::read_to_string(&path).unwrap_or_default();
    let pid = pid_str.trim();
    if pid.is_empty() {
        return Ok(());
    }
    // Check if the process is actually alive
    let alive = Command::new("kill")
        .args(["-0", pid])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);
    if alive {
        Err(format!("mole already running (pid={pid}). Use 'mole down' to stop it first."))
    } else {
        // Stale PID file — clean up
        fs::remove_file(&path).ok();
        Ok(())
    }
}

// ── Structured logging ──────────────────────────────────────────────

/// Append a timestamped line to ~/.mole/mole.log.
pub fn mole_log(level: &str, msg: &str) {
    let timestamp = chrono::Local::now().format("%Y-%m-%dT%H:%M:%S");
    let path = mole_dir().join("mole.log");
    if let Ok(mut f) = fs::File::options().create(true).append(true).open(&path) {
        writeln!(f, "{timestamp} [{level}] {msg}").ok();
    }
}

// ── TUN cleanup ────────────────────────────────────────────────────

/// Clean up orphaned TUN interfaces left by crashed sing-box.
/// Must be called before starting a new sing-box instance.
pub fn cleanup_tun() {
    #[cfg(target_os = "macos")]
    {
        // On macOS, sing-box creates utun* interfaces. Find and remove them.
        if let Ok(output) = Command::new("ifconfig").output() {
            let text = String::from_utf8_lossy(&output.stdout);
            for line in text.lines() {
                // utun interfaces created by sing-box have 172.19.0.1
                if line.starts_with("utun") && line.contains("flags=") {
                    let iface = line.split(':').next().unwrap_or("");
                    if !iface.is_empty() {
                        // Check if this interface has our TUN address
                        if let Ok(detail) = Command::new("ifconfig").arg(iface).output() {
                            let detail_str = String::from_utf8_lossy(&detail.stdout);
                            if detail_str.contains("172.19.0.1") {
                                let _ = Command::new("sudo")
                                    .args(["-n", "ifconfig", iface, "destroy"])
                                    .output();
                                mole_log("INFO", &format!("cleaned up orphaned TUN: {iface}"));
                            }
                        }
                    }
                }
            }
        }
    }
    #[cfg(target_os = "linux")]
    {
        // On Linux, explicitly delete the tun interface
        let _ = Command::new("sudo")
            .args(["-n", "ip", "link", "delete", "tun0"])
            .output();
    }
}

// ── Config validation ───────────────────────────────────────────────

/// Check that geo rule set files exist, with actionable error messages.
pub fn check_geo_files(rules: &[crate::store::Rule], bypass_cn: bool) -> Result<(), String> {
    let dir = mole_dir();
    let mut missing = Vec::new();

    if bypass_cn {
        for name in &["geoip-cn.srs", "geosite-cn.srs"] {
            if !dir.join(name).exists() {
                missing.push(name.to_string());
            }
        }
    }

    for rule in rules {
        let file = match rule.match_type.as_str() {
            "geoip" => format!("geoip-{}.srs", rule.pattern),
            "geosite" => format!("geosite-{}.srs", rule.pattern),
            _ => continue,
        };
        if !dir.join(&file).exists() && !missing.contains(&file) {
            missing.push(file);
        }
    }

    if missing.is_empty() {
        return Ok(());
    }

    let dir_str = dir.display();
    let mut msg = format!("missing geo rule files in {dir_str}/:\n");
    for f in &missing {
        msg.push_str(&format!("  - {f}\n"));
    }
    msg.push_str("download from: https://github.com/SagerNet/sing-geoip/releases (geoip-*.srs)\n");
    msg.push_str(
        "           or: https://github.com/SagerNet/sing-geosite/releases (geosite-*.srs)",
    );
    Err(msg)
}

/// Run `sing-box check -c <path>` to validate config before starting.
pub fn check_config(config_path: &std::path::Path) -> Result<(), String> {
    let bin = singbox_bin_path();
    if !bin.exists() {
        return Err("sing-box binary not found".into());
    }
    let bin_str = bin.to_str().ok_or("non-UTF8 binary path")?;
    let cfg_str = config_path.to_str().ok_or("non-UTF8 config path")?;
    let output = Command::new(bin_str)
        .args(["check", "-c", cfg_str])
        .output()
        .map_err(|e| format!("config check: {e}"))?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        return Err(format!("invalid config: {}", stderr.trim()));
    }
    Ok(())
}

// ── Install ─────────────────────────────────────────────────────────

pub fn install_singbox() -> Result<(), String> {
    let arch = platform::arch_name();
    let os = platform::os_name();

    let tarball = format!("sing-box-{SINGBOX_VERSION}-{os}-{arch}.tar.gz");
    let url = format!(
        "https://github.com/SagerNet/sing-box/releases/download/v{SINGBOX_VERSION}/{tarball}"
    );

    println!("downloading sing-box {SINGBOX_VERSION} ({arch})...");
    println!("  {url}");

    let resp = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(300))
        .build()
        .map_err(|e| format!("http client: {e}"))?
        .get(&url)
        .send()
        .map_err(|e| format!("download failed: {e}"))?;

    if !resp.status().is_success() {
        return Err(format!("download failed: HTTP {}", resp.status()));
    }

    let bytes = resp.bytes().map_err(|e| format!("read failed: {e}"))?;

    // Write tar.gz to temp file
    let tmp_tar = mole_dir().join(&tarball);
    let mut file = fs::File::create(&tmp_tar).map_err(|e| format!("create temp: {e}"))?;
    file.write_all(&bytes)
        .map_err(|e| format!("write temp: {e}"))?;
    drop(file);

    // Extract sing-box binary
    let extract_dir = format!("sing-box-{SINGBOX_VERSION}-{os}-{arch}");
    let status = Command::new("tar")
        .args([
            "xzf",
            tmp_tar.to_str().unwrap(),
            "-C",
            mole_dir().to_str().unwrap(),
            &format!("{extract_dir}/sing-box"),
        ])
        .status()
        .map_err(|e| format!("tar: {e}"))?;

    if !status.success() {
        return Err("tar extraction failed".into());
    }

    // Move binary to bin/
    let extracted = mole_dir().join(&extract_dir).join("sing-box");
    let dest = singbox_bin_path();
    fs::rename(&extracted, &dest).map_err(|e| format!("move binary: {e}"))?;

    // Cleanup
    fs::remove_dir_all(mole_dir().join(&extract_dir)).ok();
    fs::remove_file(&tmp_tar).ok();

    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&dest, fs::Permissions::from_mode(0o755))
            .map_err(|e| format!("chmod: {e}"))?;
    }

    println!("installed to {}", dest.display());
    Ok(())
}

// ── Config I/O ──────────────────────────────────────────────────────

pub fn write_config(json: &str) -> Result<PathBuf, String> {
    let path = config_path();
    fs::write(&path, json).map_err(|e| format!("write config: {e}"))?;
    Ok(path)
}

// ── Process management ──────────────────────────────────────────────

/// Stop sing-box: SIGTERM → wait → SIGKILL.
/// When SHUTTING_DOWN is set (Ctrl+C), skip graceful wait and use SIGKILL directly.
pub fn stop_singbox() -> Result<bool, String> {
    if SHUTTING_DOWN.load(Ordering::SeqCst) {
        // Fast path: SIGKILL immediately
        let _ = Command::new("sudo")
            .args(["-n", "pkill", "-9", "sing-box"])
            .output();
        std::thread::sleep(Duration::from_millis(500));
        cleanup_tun();
        SINGBOX_PID.store(0, Ordering::SeqCst);
        return Ok(true);
    }

    // Graceful: SIGTERM first, sudo (sing-box runs as root)
    let term = Command::new("sudo")
        .args(["-n", "pkill", "-TERM", "sing-box"])
        .output();

    let term_success = term.as_ref().map(|o| o.status.success()).unwrap_or(false);
    if !term_success {
        SINGBOX_PID.store(0, Ordering::SeqCst);
        return Ok(false); // No process found
    }

    std::thread::sleep(Duration::from_millis(1500));

    // Check if still alive
    let alive = Command::new("pgrep")
        .args(["-f", "sing-box run"])
        .output()
        .map(|o| o.status.success())
        .unwrap_or(false);

    if alive {
        let _ = Command::new("sudo")
            .args(["-n", "pkill", "-9", "sing-box"])
            .output();
        std::thread::sleep(Duration::from_millis(500));
    }

    cleanup_tun();
    SINGBOX_PID.store(0, Ordering::SeqCst);
    Ok(true)
}

// ── Keepalive + health watchdog ─────────────────────────────────────

/// Keepalive failure threshold before triggering restart.
const KEEPALIVE_THRESHOLD: u32 = 3;
/// Normal keepalive interval (when healthy).
const KEEPALIVE_INTERVAL: Duration = Duration::from_secs(20);
/// Fast-retry interval after a failure (adaptive probing).
const KEEPALIVE_RETRY_INTERVAL: Duration = Duration::from_secs(5);
/// HTTP probe timeout.
const KEEPALIVE_HTTP_TIMEOUT: Duration = Duration::from_secs(8);
/// Grace period after sing-box start: must observe at least one successful
/// probe before the watchdog starts counting failures. Avoids killing a
/// node that simply takes longer than usual to finish handshake.
const KEEPALIVE_BOOTSTRAP_TIMEOUT: Duration = Duration::from_secs(90);

/// Probe actual end-to-end connectivity through the proxy via HTTP.
/// Unlike UDP DNS (which gets hijacked by sing-box and may return cached results),
/// this makes a real TCP connection through the TUN to a remote server,
/// proving the full path: TUN → sing-box → proxy → internet → response.
///
/// Races several endpoints in parallel; any one success means the tunnel is alive.
/// Endpoints are chosen to be reachable from both CN and non-CN egress and to
/// tolerate individual domains being censored, rate-limited, or load-shedding.
fn probe_connectivity() -> bool {
    let endpoints = [
        "http://www.gstatic.com/generate_204",
        "http://www.google.com/generate_204",
        "http://captive.apple.com",
        "http://cp.cloudflare.com",
    ];

    let (tx, rx) = std::sync::mpsc::channel();
    for url in endpoints {
        let tx = tx.clone();
        std::thread::spawn(move || {
            let ok = reqwest::blocking::Client::builder()
                .timeout(KEEPALIVE_HTTP_TIMEOUT)
                .no_proxy()
                .build()
                .ok()
                .and_then(|c| c.get(url).send().ok())
                .map(|r| r.status().is_success() || r.status().as_u16() == 204)
                .unwrap_or(false);
            let _ = tx.send(ok);
        });
    }
    drop(tx);

    let deadline = Instant::now() + KEEPALIVE_HTTP_TIMEOUT;
    while let Some(remaining) = deadline.checked_duration_since(Instant::now()) {
        match rx.recv_timeout(remaining) {
            Ok(true) => return true,
            Ok(false) => continue,
            Err(_) => break,
        }
    }
    false
}

/// Background keepalive: probes real HTTP connectivity every 20s.
/// After KEEPALIVE_THRESHOLD consecutive failures, triggers sing-box restart.
/// Uses adaptive probing: retries faster (5s) after first failure for quick recovery.
pub fn start_keepalive(stop: Arc<AtomicBool>) -> std::thread::JoinHandle<()> {
    std::thread::spawn(move || {
        let mut consecutive_failures: u32 = 0;
        // Bootstrap gate: arm the watchdog only after the first successful probe
        // (or after KEEPALIVE_BOOTSTRAP_TIMEOUT elapses, whichever comes first).
        // This prevents a slow-to-handshake node from being killed during startup.
        let started_at = Instant::now();
        let mut armed = false;

        // Initial delay: let sing-box establish tunnel before checking
        std::thread::sleep(Duration::from_secs(15));

        loop {
            if stop.load(Ordering::SeqCst) || SHUTTING_DOWN.load(Ordering::SeqCst) {
                break;
            }

            if probe_connectivity() {
                if !armed {
                    armed = true;
                    mole_log("INFO", "keepalive: bootstrap probe succeeded, watchdog armed");
                } else if consecutive_failures > 0 {
                    mole_log(
                        "INFO",
                        &format!("keepalive: recovered after {consecutive_failures} failures"),
                    );
                }
                consecutive_failures = 0;
            } else if !armed {
                // Still in bootstrap: don't count failures, don't restart.
                // Arm automatically once the bootstrap timeout elapses so a
                // permanently-broken node can still be recovered.
                if started_at.elapsed() >= KEEPALIVE_BOOTSTRAP_TIMEOUT {
                    armed = true;
                    mole_log(
                        "WARN",
                        "keepalive: bootstrap window expired without a successful probe, watchdog armed",
                    );
                } else {
                    mole_log("DEBUG", "keepalive: probe failed during bootstrap (ignored)");
                }
            } else {
                consecutive_failures += 1;
                mole_log(
                    "WARN",
                    &format!("keepalive: probe failed ({consecutive_failures}/{KEEPALIVE_THRESHOLD})"),
                );

                if consecutive_failures >= KEEPALIVE_THRESHOLD {
                    mole_log("WARN", "keepalive: tunnel dead, triggering restart");
                    HEALTH_KILL.store(true, Ordering::SeqCst);
                    stop_singbox().ok();
                    consecutive_failures = 0;
                    std::thread::sleep(Duration::from_secs(10));
                }
            }

            // Adaptive interval: fast retry on failure, normal when healthy
            let interval = if consecutive_failures > 0 {
                KEEPALIVE_RETRY_INTERVAL
            } else {
                KEEPALIVE_INTERVAL
            };
            std::thread::sleep(interval);
        }
    })
}

// ── Log rotation ────────────────────────────────────────────────────

fn rotate_log() {
    let current = log_path();
    if current.exists() {
        let prev = prev_log_path();
        fs::rename(&current, &prev).ok();
    }
}

// ── Spawn & run ─────────────────────────────────────────────────────

fn spawn_singbox(config_path: &std::path::Path, retry_num: u32) -> Result<Child, String> {
    let bin = singbox_bin_path();
    let log_file = log_path();

    let log = if retry_num == 0 {
        rotate_log();
        fs::File::create(&log_file).map_err(|e| format!("create log: {e}"))?
    } else {
        let mut f = fs::File::options()
            .create(true)
            .append(true)
            .open(&log_file)
            .map_err(|e| format!("open log: {e}"))?;
        writeln!(
            f,
            "\n--- sing-box restart #{retry_num} at {} ---",
            chrono::Local::now().format("%Y-%m-%d %H:%M:%S")
        )
        .ok();
        f
    };
    let log_err = log.try_clone().map_err(|e| format!("clone log: {e}"))?;

    let bin_str = bin.to_str().ok_or("non-UTF8 binary path")?;
    let cfg_str = config_path.to_str().ok_or("non-UTF8 config path")?;
    mole_log(
        "DEBUG",
        &format!("spawn cmd: sudo {bin_str} run -c {cfg_str} (retry={retry_num})"),
    );
    let child = Command::new("sudo")
        .arg(bin_str)
        .arg("run")
        .arg("-c")
        .arg(cfg_str)
        .stdout(log)
        .stderr(log_err)
        .spawn()
        .map_err(|e| format!("failed to spawn sing-box: {e}"))?;

    Ok(child)
}

/// Read the last N lines of sing-box.log for post-mortem diagnosis.
fn tail_singbox_log(n: usize) -> Vec<String> {
    let Ok(content) = fs::read_to_string(log_path()) else {
        return Vec::new();
    };
    content
        .lines()
        .rev()
        .take(n)
        .map(|s| {
            s.replace("\x1b[31m", "")
                .replace("\x1b[0m", "")
                .replace("\x1b[36m", "")
                .replace("\x1b[33m", "")
                .trim()
                .to_string()
        })
        .filter(|s| !s.is_empty())
        .collect::<Vec<_>>()
        .into_iter()
        .rev()
        .collect()
}

/// Classify a sing-box exit by inspecting the log tail and return a short hint.
/// Returns None if no well-known pattern is detected.
fn classify_exit(tail: &[String]) -> Option<&'static str> {
    let blob = tail.join("\n").to_lowercase();
    if blob.contains("sudo: a password is required")
        || blob.contains("sudo: a terminal is required")
    {
        return Some("sudo needs a password — run `sudo -v` in your terminal first, or configure NOPASSWD for sing-box");
    }
    if blob.contains("operation not permitted") {
        return Some("permission denied creating TUN — sing-box must run as root");
    }
    if blob.contains("address already in use") {
        return Some("port already bound — another sing-box/mole may still be alive");
    }
    if blob.contains("no such file or directory") && blob.contains(".srs") {
        return Some("geo rule set missing — run `mole install` or download .srs files");
    }
    if blob.contains("connection refused") || blob.contains("i/o timeout") {
        return Some("upstream unreachable — node may be down, try `mole bench` to pick another");
    }
    None
}

/// Run sing-box with auto-restart, exponential backoff, and health-kill awareness.
pub fn run_singbox(config_path: &std::path::Path) -> Result<ExitReason, String> {
    let bin = singbox_bin_path();
    if !bin.exists() {
        return Err("sing-box binary not found, run `mole install` first".into());
    }

    let mut retries: u32 = 0;

    loop {
        // Kill any orphaned sing-box and clean up TUN before each spawn
        let _ = Command::new("sudo")
            .args(["-n", "pkill", "-9", "sing-box"])
            .output();
        std::thread::sleep(Duration::from_millis(500));
        cleanup_tun();

        let mut child = spawn_singbox(config_path, retries)?;
        let pid = child.id();
        SINGBOX_PID.store(pid, Ordering::SeqCst);
        mole_log("INFO", &format!("sing-box spawned pid={pid}"));

        let started_at = Instant::now();
        let status = child.wait().map_err(|e| format!("wait: {e}"))?;
        let uptime = started_at.elapsed();
        SINGBOX_PID.store(0, Ordering::SeqCst);

        mole_log(
            "DEBUG",
            &format!(
                "sing-box wait returned: code={:?} signal={:?} uptime={}s shutting_down={} health_kill={}",
                status.code(),
                {
                    #[cfg(unix)]
                    { status.signal() }
                    #[cfg(not(unix))]
                    { None::<i32> }
                },
                uptime.as_secs(),
                SHUTTING_DOWN.load(Ordering::SeqCst),
                HEALTH_KILL.load(Ordering::SeqCst),
            ),
        );

        // User-initiated shutdown
        if SHUTTING_DOWN.load(Ordering::SeqCst) {
            cleanup_tun();
            return Ok(ExitReason::Clean);
        }

        // Clean exit code
        if status.success() {
            return Ok(ExitReason::Clean);
        }

        // Check if this was a health-triggered kill (not user-initiated)
        let was_health_kill = HEALTH_KILL.swap(false, Ordering::SeqCst);

        #[cfg(unix)]
        if let Some(sig) = status.signal() {
            if !was_health_kill && (sig == 2 || sig == 15) {
                // SIGINT/SIGTERM from user — clean exit
                cleanup_tun();
                return Ok(ExitReason::Clean);
            }
            mole_log(
                "WARN",
                &format!(
                    "sing-box killed by signal {sig} (health_kill={was_health_kill}, uptime={}s)",
                    uptime.as_secs()
                ),
            );
            eprintln!("\nsing-box killed by signal {sig}");
        }

        // Capture sing-box crash reason from its log
        let tail = tail_singbox_log(15);
        if tail.is_empty() {
            mole_log(
                "WARN",
                "sing-box.log is empty — likely died before writing (sudo denied or exec failed)",
            );
        } else {
            // Always dump the tail so failures without FATAL/ERROR markers (e.g. sudo) are visible
            for line in &tail {
                mole_log("TRACE", &format!("sing-box: {line}"));
            }
            for line in &tail {
                if line.contains("FATAL") || line.contains("ERROR") {
                    mole_log("ERROR", &format!("sing-box: {line}"));
                    eprintln!("  \x1b[31m{line}\x1b[0m");
                }
            }
        }
        if let Some(hint) = classify_exit(&tail) {
            mole_log("ERROR", &format!("diagnosis: {hint}"));
            eprintln!("  \x1b[33mhint: {hint}\x1b[0m");
        }

        // Reset retry counter if the process ran for >5 minutes (was stable)
        if uptime > Duration::from_secs(300) {
            mole_log(
                "INFO",
                &format!(
                    "resetting retry counter (ran {}s before crash)",
                    uptime.as_secs()
                ),
            );
            retries = 0;
        }

        retries += 1;
        if retries > MAX_RETRIES {
            mole_log(
                "ERROR",
                &format!("sing-box crashed {MAX_RETRIES} times, giving up"),
            );
            eprintln!("\nsing-box crashed {MAX_RETRIES} times, giving up");
            eprintln!("check log: {}", log_path().display());
            cleanup_tun();
            return Ok(ExitReason::MaxRetries);
        }

        // Exponential backoff: 2, 4, 8, 16, 32, 60, 60…
        let wait = std::cmp::min(2u64.pow(retries), 60);
        mole_log(
            "INFO",
            &format!(
                "sing-box exited (code={}), restarting in {wait}s ({retries}/{MAX_RETRIES})",
                status
                    .code()
                    .map(|c| c.to_string())
                    .unwrap_or_else(|| "signal".into())
            ),
        );
        eprintln!(
            "\nsing-box exited (code={}), restarting in {wait}s... ({retries}/{MAX_RETRIES})",
            status
                .code()
                .map(|c| c.to_string())
                .unwrap_or_else(|| "signal".into())
        );
        std::thread::sleep(Duration::from_secs(wait));
    }
}
