use serde::{Deserialize, Serialize};
use std::fs;
use std::path::PathBuf;

#[derive(Serialize, Deserialize, Clone)]
pub struct Node {
    pub name: String,
    pub uri: String,
    pub active: bool,
}

#[derive(Serialize, Deserialize, Default)]
pub struct Store {
    pub nodes: Vec<Node>,
    #[serde(default = "default_bypass_cn")]
    pub bypass_cn: bool,
}

fn default_bypass_cn() -> bool {
    true
}

fn store_path() -> PathBuf {
    let dir = dirs::home_dir()
        .expect("cannot find home directory")
        .join(".mole");
    fs::create_dir_all(&dir).ok();
    dir.join("nodes.json")
}

impl Store {
    pub fn load() -> Self {
        let path = store_path();
        if !path.exists() {
            return Store {
                nodes: vec![],
                bypass_cn: true,
            };
        }
        let data = fs::read_to_string(&path).unwrap_or_default();
        serde_json::from_str(&data).unwrap_or_default()
    }

    pub fn save(&self) -> Result<(), String> {
        let path = store_path();
        let json = serde_json::to_string_pretty(self).map_err(|e| format!("serialize: {e}"))?;
        fs::write(&path, json).map_err(|e| format!("write: {e}"))?;
        Ok(())
    }

    pub fn add(&mut self, name: String, uri: String) {
        // Deactivate all, activate the new one
        for n in &mut self.nodes {
            n.active = false;
        }
        // Replace if same name exists
        if let Some(existing) = self.nodes.iter_mut().find(|n| n.name == name) {
            existing.uri = uri;
            existing.active = true;
        } else {
            self.nodes.push(Node {
                name,
                uri,
                active: true,
            });
        }
    }

    pub fn active_node(&self) -> Option<&Node> {
        self.nodes.iter().find(|n| n.active)
    }

    pub fn select(&mut self, name: &str) -> bool {
        let found = self.nodes.iter().any(|n| n.name == name);
        if found {
            for n in &mut self.nodes {
                n.active = n.name == name;
            }
        }
        found
    }

    pub fn remove(&mut self, name: &str) -> bool {
        let len = self.nodes.len();
        self.nodes.retain(|n| n.name != name);
        self.nodes.len() < len
    }
}
