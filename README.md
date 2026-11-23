# dgett

## Permissions and Setup

Create the dgett-users group and add your user:

```bash
sudo groupadd --system dgett-users
sudo usermod -aG dgett-users myuser
newgrp dgett-users
```

Create the dgett user for all deployed apps:

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin dgett
sudo mkdir -p /var/lib/dgett/home
sudo chown dgett:dgett /var/lib/dgett/home
sudo mkdir -p -m 750 /var/lib/dgett/data
sudo chown root:dgett /var/lib/dgett/data
sudo usermod -aG dgett caddy
```

dgett requires a single bare git repo:

```bash
sudo git init --bare --shared=group /var/lib/dgett/repo.git
sudo chgrp -R dgett-users /var/lib/dgett/repo.git
sudo chmod -R g+rwX /var/lib/dgett/repo.git
sudo git config --system --add safe.directory /var/lib/dgett/repo.git
```

## Install dgett as systemd service

```bash
go build -o dgett .
sudo install -m 755 dgett /usr/local/bin/dgett
sudo cp dgett.service /etc/systemd/system/dgett.service
sudo systemctl daemon-reload
sudo systemctl enable --now dgett
```

Verify it is running:

```bash
systemctl status dgett
journalctl -u dgett -f
```

## Update existing dgett installation

```bash
go build -o dgett . && \
sudo install -m 755 dgett /usr/local/bin/dgett && \
sudo systemctl restart dgett
```

## Caddy

If Firefox complains that your site is risky, you may need to add Caddy's root certificate to Firefox's store and restart Firefox:

```bash
sudo cp /var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt ~/caddy-root.crt && sudo chown $USER ~/caddy-root.crt
for d in ~/.mozilla/firefox/*.default* ~/.config/mozilla/firefox/*.default*; do [ -f "$d/cert9.db" ] && certutil -A -n "Caddy Local CA" -t "C,," -i ~/caddy-root.crt -d sql:"$d"; done
```
