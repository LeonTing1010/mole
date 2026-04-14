use crate::uri::ProxyNode;
use serde::{Deserialize, Serialize};
use std::fs;
use std::path::PathBuf;

#[derive(Serialize, Deserialize, Clone)]
pub struct Node {
    pub name: String,
    pub uri: String,
    pub active: bool,
    /// Which subscription this node came from (None = manually added).
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub source: Option<String>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct Subscription {
    pub name: String,
    pub url: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub last_update: Option<String>,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct Rule {
    pub match_type: String, // domain, domain_suffix, domain_keyword, ip_cidr, geoip, geosite
    pub pattern: String,
    pub outbound: String, // direct, block, or node name
}

#[derive(Serialize, Deserialize, Default)]
pub struct Store {
    pub nodes: Vec<Node>,
    #[serde(default = "default_bypass_cn")]
    pub bypass_cn: bool,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub subscriptions: Vec<Subscription>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub rules: Vec<Rule>,
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

// ── Bench results (separate from node config) ──────────────

#[derive(Serialize, Deserialize, Clone)]
pub struct BenchEntry {
    pub speed_kbps: u64,
    pub ipv6: bool,
}

/// Bench results stored in ~/.mole/bench.json, keyed by node name.
pub type BenchResults = std::collections::HashMap<String, BenchEntry>;

fn bench_path() -> PathBuf {
    let dir = dirs::home_dir()
        .expect("cannot find home directory")
        .join(".mole");
    dir.join("bench.json")
}

pub fn load_bench() -> BenchResults {
    let path = bench_path();
    if !path.exists() {
        return BenchResults::new();
    }
    let data = fs::read_to_string(&path).unwrap_or_default();
    serde_json::from_str(&data).unwrap_or_default()
}

pub fn save_bench(results: &BenchResults) {
    let path = bench_path();
    if let Ok(json) = serde_json::to_string_pretty(results) {
        fs::write(&path, json).ok();
    }
}

// ── Discover sources (separate from subscriptions) ─────────

#[derive(Serialize, Deserialize, Clone)]
pub struct DiscoverSource {
    pub name: String,
    pub url: String,
    /// "subscription", "html", or "date-pattern"
    #[serde(default = "default_source_type")]
    pub source_type: String,
    /// For date-pattern: number of files (0..count), default 1
    #[serde(default, skip_serializing_if = "is_zero")]
    pub count: usize,
}

fn is_zero(v: &usize) -> bool {
    *v == 0
}

fn default_source_type() -> String {
    "subscription".to_string()
}

fn sources_path() -> PathBuf {
    let dir = dirs::home_dir()
        .expect("cannot find home directory")
        .join(".mole");
    dir.join("sources.json")
}

pub fn load_sources() -> Vec<DiscoverSource> {
    let path = sources_path();
    if !path.exists() {
        return vec![];
    }
    let data = fs::read_to_string(&path).unwrap_or_default();
    serde_json::from_str(&data).unwrap_or_default()
}

pub fn save_sources(sources: &[DiscoverSource]) {
    let path = sources_path();
    if let Ok(json) = serde_json::to_string_pretty(sources) {
        fs::write(&path, json).ok();
    }
}

impl Store {
    pub fn load() -> Self {
        let path = store_path();
        if !path.exists() {
            return Store {
                nodes: vec![],
                bypass_cn: true,
                subscriptions: vec![],
                rules: vec![],
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

    // ── Node methods ────────────────────────────────────────────

    pub fn add(&mut self, name: String, uri: String) {
        for n in &mut self.nodes {
            n.active = false;
        }
        if let Some(existing) = self.nodes.iter_mut().find(|n| n.name == name) {
            existing.uri = uri;
            existing.active = true;
            existing.source = None;
        } else {
            self.nodes.push(Node {
                name,
                uri,
                active: true,
                source: None,
            });
        }
    }

    /// Add a node with a source tag (for discover). Does not deactivate other nodes.
    pub fn add_with_source(&mut self, name: String, uri: String, source: &str) {
        if let Some(existing) = self.nodes.iter_mut().find(|n| n.name == name) {
            existing.uri = uri;
            existing.source = Some(source.to_string());
        } else {
            self.nodes.push(Node {
                name,
                uri,
                active: false,
                source: Some(source.to_string()),
            });
        }
    }

    pub fn active_node(&self) -> Option<&Node> {
        self.nodes.iter().find(|n| n.active)
    }

    /// Find a node index by: exact name, 1-based index, or case-insensitive substring.
    /// Returns `Ok(index)` on unique match, `Err(candidates)` if ambiguous (multiple matches).
    pub fn find_node(&self, query: &str) -> Result<usize, Vec<(usize, &str)>> {
        // 1. Exact match
        if let Some(i) = self.nodes.iter().position(|n| n.name == query) {
            return Ok(i);
        }
        // 2. Numeric index (1-based)
        if let Ok(idx) = query.parse::<usize>() {
            if idx >= 1 && idx <= self.nodes.len() {
                return Ok(idx - 1);
            }
        }
        // 3. Case-insensitive substring
        let lower = query.to_lowercase();
        let matches: Vec<(usize, &str)> = self
            .nodes
            .iter()
            .enumerate()
            .filter(|(_, n)| n.name.to_lowercase().contains(&lower))
            .map(|(i, n)| (i, n.name.as_str()))
            .collect();
        match matches.len() {
            1 => Ok(matches[0].0),
            0 => Err(vec![]),
            _ => Err(matches),
        }
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

    pub fn select_by_index(&mut self, idx: usize) {
        for (i, n) in self.nodes.iter_mut().enumerate() {
            n.active = i == idx;
        }
    }

    pub fn remove(&mut self, name: &str) -> bool {
        let len = self.nodes.len();
        self.nodes.retain(|n| n.name != name);
        self.nodes.len() < len
    }

    pub fn remove_by_index(&mut self, idx: usize) -> String {
        let name = self.nodes[idx].name.clone();
        self.nodes.remove(idx);
        name
    }

    /// Get the next node after `current_name` (wraps around). Returns None if ≤1 node.
    pub fn next_node(&self, current_name: &str) -> Option<&Node> {
        if self.nodes.len() <= 1 {
            return None;
        }
        let idx = self.nodes.iter().position(|n| n.name == current_name)?;
        let next_idx = (idx + 1) % self.nodes.len();
        Some(&self.nodes[next_idx])
    }

    // ── Subscription methods ────────────────────────────────────

    pub fn add_subscription(&mut self, name: String, url: String) {
        if let Some(existing) = self.subscriptions.iter_mut().find(|s| s.name == name) {
            existing.url = url;
        } else {
            self.subscriptions.push(Subscription {
                name,
                url,
                last_update: None,
            });
        }
    }

    pub fn remove_subscription(&mut self, name: &str) -> bool {
        let before = self.subscriptions.len();
        self.subscriptions.retain(|s| s.name != name);
        self.nodes.retain(|n| n.source.as_deref() != Some(name));
        self.subscriptions.len() < before
    }

    /// Replace all nodes from a subscription with new ones.
    /// Preserves active state if the active node still exists after update.
    /// Deduplicates by prefixing with subscription name when names collide.
    pub fn update_subscription_nodes(&mut self, sub_name: &str, uris: Vec<(String, String)>) {
        // Remember active node
        let active_name = self.active_node().map(|n| n.name.clone());

        // Remove old nodes from this subscription
        self.nodes.retain(|n| n.source.as_deref() != Some(sub_name));

        // Collect existing names (from other sources) for dedup
        let existing_names: std::collections::HashSet<String> =
            self.nodes.iter().map(|n| n.name.clone()).collect();

        // Deduplicate by server address within this subscription batch
        let mut seen_keys = std::collections::HashSet::new();
        for (name, uri) in uris {
            let key = ProxyNode::parse(&uri)
                .map(|n| format!("{}:{}", n.protocol(), n.server_addr()))
                .unwrap_or_else(|_| uri.clone());
            if !seen_keys.insert(key) {
                continue; // skip duplicate server
            }
            let final_name = if existing_names.contains(&name) {
                format!("{name} [{sub_name}]")
            } else {
                name
            };
            self.nodes.push(Node {
                name: final_name,
                uri,
                active: false,
                source: Some(sub_name.to_string()),

            });
        }

        // Restore active state
        if let Some(ref name) = active_name {
            if self.nodes.iter().any(|n| n.name == *name) {
                self.select(name);
            }
        }

        if let Some(sub) = self.subscriptions.iter_mut().find(|s| s.name == sub_name) {
            sub.last_update = Some(chrono::Local::now().format("%Y-%m-%d %H:%M:%S").to_string());
        }
    }

    // ── Rule methods ────────────────────────────────────────────

    pub fn add_rule(&mut self, match_type: String, pattern: String, outbound: String) {
        self.rules.push(Rule {
            match_type,
            pattern,
            outbound,
        });
    }

    pub fn remove_rule(&mut self, index: usize) -> bool {
        if index < self.rules.len() {
            self.rules.remove(index);
            true
        } else {
            false
        }
    }

    pub fn clear_rules(&mut self) {
        self.rules.clear();
    }

}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_store() -> Store {
        Store {
            nodes: vec![
                Node {
                    name: "n1".into(),
                    uri: "trojan://p@a.com:443#n1".into(),
                    active: true,
                    source: Some("sub1".into()),
    
                },
                Node {
                    name: "n2".into(),
                    uri: "trojan://p@b.com:443#n2".into(),
                    active: false,
                    source: Some("sub1".into()),
    
                },
                Node {
                    name: "manual".into(),
                    uri: "trojan://p@c.com:443#manual".into(),
                    active: false,
                    source: None,
    
                },
            ],
            bypass_cn: true,
            subscriptions: vec![Subscription {
                name: "sub1".into(),
                url: "https://example.com".into(),
                last_update: None,
            }],
            rules: vec![],
        }
    }

    #[test]
    fn update_subscription_preserves_active() {
        let mut s = test_store();
        assert_eq!(s.active_node().unwrap().name, "n1");

        // Update subscription — n1 still exists in new data
        let new_nodes = vec![
            ("n1".into(), "trojan://p@a-new.com:443#n1".into()),
            ("n3".into(), "trojan://p@d.com:443#n3".into()),
        ];
        s.update_subscription_nodes("sub1", new_nodes);

        // n1 should still be active
        assert_eq!(s.active_node().unwrap().name, "n1");
        // manual node should be untouched
        assert!(s.nodes.iter().any(|n| n.name == "manual"));
        // n2 should be gone (not in new data)
        assert!(!s.nodes.iter().any(|n| n.name == "n2"));
    }

    #[test]
    fn update_subscription_deduplicates_names() {
        let mut s = test_store();
        // Add nodes from sub2 that collide with "manual" from another source
        s.add_subscription("sub2".into(), "https://other.com".into());
        let new_nodes = vec![("manual".into(), "trojan://p@x.com:443#manual".into())];
        s.update_subscription_nodes("sub2", new_nodes);

        // Should have "manual" (original) and "manual [sub2]" (deduped)
        let names: Vec<&str> = s.nodes.iter().map(|n| n.name.as_str()).collect();
        assert!(names.contains(&"manual"));
        assert!(names.contains(&"manual [sub2]"));
    }

    #[test]
    fn remove_subscription_removes_its_nodes() {
        let mut s = test_store();
        assert_eq!(s.nodes.len(), 3);
        s.remove_subscription("sub1");
        // Only manual node should remain
        assert_eq!(s.nodes.len(), 1);
        assert_eq!(s.nodes[0].name, "manual");
    }
}
