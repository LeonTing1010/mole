use url::Url;

#[derive(Debug, Clone)]
pub struct Hy2Uri {
    pub auth: String,
    pub host: String,
    pub port: u16,
    pub sni: Option<String>,
    pub insecure: bool,
    pub hop_ports: Option<String>,
    pub obfs: Option<String>,
    pub obfs_password: Option<String>,
    pub name: Option<String>,
}

impl Hy2Uri {
    pub fn parse(input: &str) -> Result<Self, String> {
        let normalized = if input.starts_with("hy2://") {
            input.replacen("hy2://", "hysteria2://", 1)
        } else {
            input.to_string()
        };

        // url crate doesn't know "hysteria2" scheme, so swap to https for parsing
        let fake = normalized.replacen("hysteria2://", "https://", 1);
        let parsed = Url::parse(&fake).map_err(|e| format!("invalid URI: {e}"))?;

        let auth = parsed.username().to_string();
        if auth.is_empty() {
            return Err("missing auth (password) in URI".into());
        }

        let host = parsed
            .host_str()
            .ok_or("missing host")?
            .to_string();

        let port = parsed.port().ok_or("missing port")?;

        let mut sni = None;
        let mut insecure = false;
        let mut hop_ports = None;
        let mut obfs = None;
        let mut obfs_password = None;

        for (k, v) in parsed.query_pairs() {
            match k.as_ref() {
                "sni" => sni = Some(v.to_string()),
                "insecure" => insecure = v == "1" || v == "true",
                "mport" => hop_ports = Some(v.to_string()),
                "obfs" => obfs = Some(v.to_string()),
                "obfs-password" => obfs_password = Some(v.to_string()),
                _ => {}
            }
        }

        let name = parsed.fragment().map(|f| {
            urlencoding::decode(f).unwrap_or_else(|_| f.into()).to_string()
        });

        Ok(Hy2Uri {
            auth,
            host,
            port,
            sni,
            insecure,
            hop_ports,
            obfs,
            obfs_password,
            name,
        })
    }

    /// Server address for hysteria2 config (with hop ports if available)
    pub fn server_addr(&self) -> String {
        if let Some(ref ports) = self.hop_ports {
            format!("{}:{}", self.host, ports)
        } else {
            format!("{}:{}", self.host, self.port)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_full_uri() {
        let uri = "hysteria2://dongtaiwang.com@195.154.54.131:40022?sni=www.microsoft.com&insecure=1&mport=41000-42000#Hysteria2%E8%8A%82%E7%82%B9";
        let parsed = Hy2Uri::parse(uri).unwrap();
        assert_eq!(parsed.auth, "dongtaiwang.com");
        assert_eq!(parsed.host, "195.154.54.131");
        assert_eq!(parsed.port, 40022);
        assert_eq!(parsed.sni.as_deref(), Some("www.microsoft.com"));
        assert!(parsed.insecure);
        assert_eq!(parsed.hop_ports.as_deref(), Some("41000-42000"));
        assert_eq!(parsed.name.as_deref(), Some("Hysteria2节点"));
        assert_eq!(parsed.server_addr(), "195.154.54.131:41000-42000");
    }
}
