# dgett

## Install dgett as systemd service

First, create a dgett group and add your user:

```bash
sudo groupadd --system dgett
sudo usermod -aG dgett myuser
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
sudo install -m 644 dgett.service /etc/systemd/system/dgett.service
sudo systemctl daemon-reload
sudo systemctl restart dgett
```
