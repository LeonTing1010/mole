package cmd

import "encoding/json"

// Protocol readiness is independent of optional tuning readiness. Never infer
// an interface qdisc from the sysctl default used for future devices.
type nodeTuningReport struct {
	Status            string            `json:"status"`
	CheckedAt         string            `json:"checked_at,omitempty"`
	Interface         string            `json:"interface,omitempty"`
	Qdisc             string            `json:"qdisc,omitempty"`
	DefaultQdisc      string            `json:"default_qdisc,omitempty"`
	CongestionControl string            `json:"default_tcp_cc,omitempty"`
	TCPFastOpen       string            `json:"tcp_fastopen,omitempty"`
	ServerTCPFastOpen bool              `json:"server_tcp_fast_open"`
	ApplyStatus       string            `json:"apply_status,omitempty"`
	Versions          map[string]string `json:"versions,omitempty"`
	Services          map[string]string `json:"services,omitempty"`
	ActiveTCPCC       []string          `json:"active_tcp_cc,omitempty"`
	Errors            []string          `json:"errors,omitempty"`
}

func decodeNodeTuning(data []byte) nodeTuningReport {
	var report nodeTuningReport
	if json.Unmarshal(data, &report) != nil || report.Interface == "" || report.Qdisc == "" || report.DefaultQdisc == "" || (report.Status != "ready" && report.Status != "degraded") {
		return nodeTuningReport{Status: "unobserved"}
	}
	return report
}

// Shared by all boot profiles. Preserve the existing queue until matched
// experiments justify a change; surface any apply failure rather than hiding it.
const nodeNetworkTuning = `
(
mkdir -p /etc/sysctl.d /var/log/mole /usr/local/lib
cat > /etc/sysctl.d/99-mole.conf << 'MOLE_SYSCTL_EOF'
net.core.rmem_max=16777216
net.core.wmem_max=16777216
# Per-socket receive guarantee under UDP memory pressure, not QUIC slow start.
net.ipv4.udp_rmem_min=8388608
net.ipv4.udp_wmem_min=8388608
net.core.netdev_max_backlog=250000
net.core.default_qdisc=fq_codel
net.ipv4.tcp_congestion_control=bbr
net.ipv4.tcp_fastopen=3
net.ipv4.tcp_tw_reuse=1
MOLE_SYSCTL_EOF
if timeout 10 sysctl -p /etc/sysctl.d/99-mole.conf; then
 echo applied > /var/log/mole/network-apply.status
else
 echo degraded > /var/log/mole/network-apply.status
 echo 'mole tuning degraded: sysctl apply failed' >&2
fi
cat > /usr/local/lib/mole-network-state.py << 'MOLE_NETWORK_STATE_EOF'
` + nodeNetworkStatePython + `
MOLE_NETWORK_STATE_EOF
chmod 644 /usr/local/lib/mole-network-state.py
if ! timeout 10 python3 /usr/local/lib/mole-network-state.py > /var/log/mole/network-tuning.json; then
 echo 'mole tuning unobserved: state collection failed' >&2
fi
) || echo 'mole tuning degraded: shared stage failed' >&2
`

// Only whitelist performance fields; never return credentials or full config.
// Called at boot and on authenticated /tuning requests for current readback.
const nodeNetworkStatePython = `import json,re,subprocess,pathlib,datetime
errors=[]
def run(args,required=True):
 try:
  p=subprocess.run(args,capture_output=True,text=True,timeout=2)
  if p.returncode and required: errors.append(args[0]+': failed')
  return p.stdout.strip() if p.returncode==0 else ''
 except (OSError,subprocess.TimeoutExpired):
  if required: errors.append(args[0]+': unavailable')
  return ''
def read(path):
 try:return pathlib.Path(path).read_text().strip()
 except OSError:return 'unobserved'
def parsed(args):
 try:return json.loads(run(args))
 except ValueError:return []
r={'checked_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'default_qdisc':run(['sysctl','-n','net.core.default_qdisc']),'default_tcp_cc':run(['sysctl','-n','net.ipv4.tcp_congestion_control']),'tcp_fastopen':run(['sysctl','-n','net.ipv4.tcp_fastopen']),'apply_status':read('/var/log/mole/network-apply.status')}
routes=parsed(['ip','-json','route','show','default'])
r['interface']=next((v.get('dev','') for v in routes if v.get('dev')),'')
queues=parsed(['tc','-json','qdisc','show','dev',r['interface']]) if r['interface'] else []
r['qdisc']=','.join(sorted(set(v.get('kind','') for v in queues if v.get('root') or v.get('parent')=='root')))
r['versions']={}
for k in ['sing-box','hysteria']:
 lines=run([k,'version'],False).splitlines()
 r['versions'][k]=next((line.strip() for line in lines if line.lower().startswith(('version:', 'sing-box version'))),next(iter(lines),'unobserved'))
r['services']={k:run(['systemctl','is-active',k],False) or 'inactive' for k in ['sing-box','hysteria-server']}
# Node CPU load — tells us whether a bigger plan would actually help.
# vc2-1c-1gb is 1 vCPU; at 2026-11-10 icn peak we saw 464 Mbps at 40–50%
# load, so going 2c wouldn't move the ceiling. If load >90% on a sustained
# run, that's the one case where a hardware upgrade is worth the extra $6/mo.
try:
    ua=run(['uptime'],False).strip()
    m=re.search(r'load average: ([0-9.]+), ([0-9.]+), ([0-9.]+)',ua)
    r['load']={'1m':float(m.group(1)),'5m':float(m.group(2)),'15m':float(m.group(3))} if m else None
except Exception: r['load']=None
try:
    v=run(['vmstat','1'],False)
    lines=[l for l in v.splitlines() if l.strip()][-1]
    parts=lines.split()
    r['cpu_idle_pct']=int(parts[-1]) if parts else None
except Exception: r['cpu_idle_pct']=None
try:
 cfg=json.loads(pathlib.Path('/etc/sing-box/config.json').read_text())
 r['server_tcp_fast_open']=any(v.get('type')=='vless' and v.get('tcp_fast_open',False) for v in cfg.get('inbounds',[]))
except (OSError,ValueError):r['server_tcp_fast_open']=False
sockets=run(['ss','-tin','state','established','(','sport','=',':443',')'],False)
r['active_tcp_cc']=[cc for cc in ['brutal','bbr','cubic','reno'] if any(line.lstrip().startswith(cc+' ') for line in sockets.splitlines())]
if not r['qdisc'] or r['qdisc']!=r['default_qdisc']:errors.append('effective qdisc differs from configured default')
if r['apply_status']!='applied':errors.append('sysctl application not confirmed')
if r['default_tcp_cc']!='bbr':errors.append('default TCP congestion control differs from bbr target')
if r['tcp_fastopen']!='3':errors.append('TCP Fast Open capability differs from target')
r['errors']=errors
r['status']='degraded' if errors else 'ready'
print(json.dumps(r))
`
