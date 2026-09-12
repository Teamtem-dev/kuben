//! The machine Kuben runs on: the address others reach it at, and files
//! only its own user may read.

use std::{
    io::Write as _,
    net::{IpAddr, UdpSocket},
    path::Path,
};

use kuben_core::config::Config;

/// The address other machines reach this one at: the source address of a
/// route towards a public IP. Nothing is sent (UDP `connect` only picks the
/// route). `None` without a route, e.g. offline.
#[must_use]
pub fn advertise_ip() -> Option<IpAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0").ok()?;
    socket.connect("1.1.1.1:53").ok()?;
    let ip = socket.local_addr().ok()?.ip();
    (!ip.is_loopback() && !ip.is_unspecified()).then_some(ip)
}

/// The console's address for links: `server.public_url`, else this
/// machine's address and the bound port.
#[must_use]
pub fn console_url(cfg: &Config) -> String {
    let host = advertise_ip().map_or_else(|| "localhost".to_owned(), |ip| ip.to_string());
    cfg.console_url_with_host(&host)
}

/// Write `content` to `path` as a new file readable by its owner only.
/// An existing file is replaced, so a leftover never keeps a wider mode.
pub fn write_owner_only(path: &Path, content: &str) -> std::io::Result<()> {
    std::fs::remove_file(path).ok();
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create_new(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt as _;
        options.mode(0o600);
    }
    options.open(path)?.write_all(content.as_bytes())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn owner_only_file_is_replaced_and_private() {
        let dir = std::env::temp_dir().join(format!("kuben-host-{}", std::process::id()));
        std::fs::create_dir_all(&dir).expect("dir");
        let file = dir.join("secret");
        write_owner_only(&file, "old\n").expect("first write");
        write_owner_only(&file, "new\n").expect("second write");
        assert_eq!(std::fs::read_to_string(&file).expect("read"), "new\n");
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt as _;
            let mode = std::fs::metadata(&file).expect("metadata").permissions().mode();
            assert_eq!(mode & 0o777, 0o600);
        }
        std::fs::remove_dir_all(&dir).ok();
    }

    #[test]
    fn console_url_falls_back_to_this_machine() {
        let mut cfg = Config::default();
        cfg.server.bind = "0.0.0.0:3000".into();
        let url = console_url(&cfg);
        assert!(url.starts_with("http://") && url.ends_with(":3000"), "{url}");
    }
}
