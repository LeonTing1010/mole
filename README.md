# mole

A tiny CLI that deploys a VPS and runs a VPN through it — 5 commands, nothing else.

Built on [sing-box](https://sing-box.sagernet.org/) with CN-direct routing baked in.

## Install

```bash
# sing-box runtime (required)
brew install sing-box

# mole itself — pick one:
go install github.com/LeonTing1010/mole/mole-go@latest        # Go toolchain
# — or —
git clone https://github.com/LeonTing1010/mole && cd mole/mole-go && sudo make install
```

## Usage

```bash
mole server deploy   # deploy a VPS (Vultr / DigitalOcean / …)
mole use <name>      # pick the active server
mole up              # connect (foreground; Ctrl+C to stop)
mole down            # disconnect
mole status          # show active server + exit IP
```

Config lives in `~/.mole/`: `config.yaml` (credentials), `servers.json` (deployed VPS list), `mole.log` (sing-box output).

## Requirements

- macOS or Linux (TUN requires root; `mole up` invokes `sudo sing-box`)
- Go 1.21+ to build from source
- sing-box 1.11+ in `$PATH`

## License

MIT
