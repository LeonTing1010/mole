use std::fs;
use std::io::Write;
use std::path::PathBuf;
use std::process::{Command, ExitStatus};

const SINGBOX_VERSION: &str = "1.13.4";

fn mole_dir() -> PathBuf {
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

pub fn install_singbox() -> Result<(), String> {
    let arch = if cfg!(target_arch = "aarch64") {
        "arm64"
    } else {
        "amd64"
    };

    let tarball = format!("sing-box-{SINGBOX_VERSION}-darwin-{arch}.tar.gz");
    let url = format!(
        "https://github.com/SagerNet/sing-box/releases/download/v{SINGBOX_VERSION}/{tarball}"
    );

    println!("downloading sing-box {SINGBOX_VERSION} ({arch})...");
    println!("  {url}");

    let resp = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(300))
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
    file.write_all(&bytes).map_err(|e| format!("write temp: {e}"))?;
    drop(file);

    // Extract sing-box binary
    let extract_dir = format!("sing-box-{SINGBOX_VERSION}-darwin-{arch}");
    let status = Command::new("tar")
        .args(["xzf", tmp_tar.to_str().unwrap(), "-C", mole_dir().to_str().unwrap(),
               &format!("{extract_dir}/sing-box")])
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

pub fn write_config(json: &str) -> Result<PathBuf, String> {
    let path = config_path();
    fs::write(&path, json).map_err(|e| format!("write config: {e}"))?;
    Ok(path)
}

pub fn stop_singbox() -> Result<bool, String> {
    let output = Command::new("sudo")
        .args(["pkill", "-f", "sing-box run"])
        .output()
        .map_err(|e| format!("pkill: {e}"))?;
    Ok(output.status.success())
}

pub fn run_singbox(config_path: &PathBuf) -> Result<ExitStatus, String> {
    let bin = singbox_bin_path();
    if !bin.exists() {
        return Err("sing-box binary not found, run `mole install` first".into());
    }

    println!("starting sing-box TUN mode (requires sudo)...");
    println!("config: {}", config_path.display());
    println!("press Ctrl+C to disconnect\n");

    Command::new("sudo")
        .arg(bin.to_str().unwrap())
        .arg("run")
        .arg("-c")
        .arg(config_path.to_str().unwrap())
        .status()
        .map_err(|e| format!("failed to run sing-box: {e}"))
}
