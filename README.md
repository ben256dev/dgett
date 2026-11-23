# dgett

## Install dgett as systemd service

First, we set up the local git repo:

```bash
sudo mkdir -p /var/lib/dgett
sudo git init --bare --shared=group /var/lib/dgett/repo.git
sudo chgrp -R dgett /var/lib/dgett/repo.git
sudo chmod -R g+rwX /var/lib/dgett/repo.git
git config --global --add safe.directory /var/lib/dgett/repo.git
```

Next, create a dgett group and add your user:

```bash
sudo groupadd --system dgett
sudo usermod -aG dgett myuser
newgrp dgett
```

```bash
go build -o dgett .
sudo install -m 755 dgett /usr/local/bin/dgett
sudo cp dgett.service /etc/systemd/system/dgett.service
sudo systemctl daemon-reload
sudo systemctl enable --now dgett
```

Verify it is runnning:

```bash
systemctl status dgett
journalctl -u dgett -f
```

## Update existing dgett systemd service

```
go build -o dgett .
sudo install -m 755 dgett /usr/local/bin/dgett
sudo systemctl restart dgett
```
