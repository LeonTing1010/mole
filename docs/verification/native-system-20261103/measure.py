"""Owned-host-route TUN experiments. Reversible OS profiles; no default-route changes."""
import concurrent.futures,datetime,hashlib,json,os,pathlib,signal,socket,stat,struct,subprocess,sys,tempfile,time
TASK_HOME=pathlib.Path('/Users/leo');CFG=TASK_HOME/'.mole/sing-box-config.json';ENGINE='/opt/homebrew/bin/sing-box';IFACE='utun30';ROOT=pathlib.Path('/tmp/mole-native-system-20261103');SUITE=sys.argv[1] if len(sys.argv)>1 else 'tun'
base=next(o for o in json.loads(CFG.read_text())['outbounds'] if o.get('type')=='vless');initial_hash=hashlib.sha256(CFG.read_bytes()).hexdigest();log=TASK_HOME/'.mole/mole.log';offset=log.stat().st_size
report={'suite':SUITE,'scope':'same-node REALITY+smux; complete owned-node payload over full isolated TUN; unbound application sockets; only two owned host routes; production remains active','started_at':datetime.datetime.now().astimezone().isoformat(),'phases':[],'checks':[],'production_config_before':initial_hash}
def run(args):return subprocess.run(args,capture_output=True,text=True,timeout=30)
def safe(s):
 for v in (base.get('server'),base.get('uuid'),base.get('tls',{}).get('reality',{}).get('public_key')):
  if v:s=s.replace(v,'<redacted>')
 return s[-1800:]
def sysget(k):
 c=run(['/usr/sbin/sysctl','-n',k]);assert c.returncode==0,'sysctl absent '+k
 return c.stdout.strip()
def powerget():
 c=run(['/usr/bin/pmset','-g','custom']).stdout;section=c.split('Battery Power:',1)[1].split('AC Power:',1)[0]
 return next(x.split()[1] for x in section.splitlines() if x.strip().startswith('lowpowermode'))
keys=['kern.ipc.maxsockbuf','net.inet.tcp.autorcvbufmax','net.inet.tcp.autosndbufmax'];original={};original_power=None
profiles={'base':None,'buffer':{'kern.ipc.maxsockbuf':'16777216','net.inet.tcp.autorcvbufmax':'8388608','net.inet.tcp.autosndbufmax':'8388608'}}
def restore():
 errors=[]
 for k in reversed(keys):
  if k in original and sysget(k)!=original[k]:
   c=run(['/usr/sbin/sysctl','-w',k+'='+original[k]])
   if c.returncode:errors.append(k)
 if original_power is not None and powerget()!=original_power:
  c=run(['/usr/bin/pmset','-b','lowpowermode',original_power])
  if c.returncode:errors.append('lowpowermode')
 return errors
def profile(name):
 assert not restore(),'could not restore original controls'
 if name=='buffer':
  for k in keys:
   c=run(['/usr/sbin/sysctl','-w',k+'='+profiles['buffer'][k]]);assert c.returncode==0,'temporary sysctl rejected '+k
 elif name=='power':
  c=run(['/usr/bin/pmset','-b','lowpowermode','0']);assert c.returncode==0,'temporary power setting rejected'
 return {'sysctls':{k:sysget(k) for k in keys},'battery_low_power':powerget()}
def freeport():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def dns(port,host):
 qname=b''.join(bytes([len(x)])+x.encode() for x in host.split('.'))+b'\x00';q=struct.pack('!HHHHHH',21911,0x100,1,0,0,0)+qname+struct.pack('!HH',1,1)
 with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as s:
  s.settimeout(3);s.sendto(q,('127.0.0.1',port));r=s.recv(4096);assert struct.unpack('!H',r[6:8])[0]==1
  return socket.inet_ntoa(r[-4:])
def curl(fake,size):
 c=run(['/usr/bin/curl','--silent','--show-error','--noproxy','*','--resolve','localhost:8098:'+fake,'--connect-timeout','5','--max-time','15','--output','/dev/null','--write-out','%{http_code} %{size_download}','http://localhost:8098/bulk?bytes='+str(size)])
 f=c.stdout.split();code=int(f[0]) if f and f[0].isdigit() else 0;n=int(f[1]) if len(f)>1 else 0
 return {'ok':c.returncode==0 and code==200 and n==size,'status':code,'bytes':n,'wanted':size,'error':safe(c.stderr)}
def cpu(pid):
 x=run(['/bin/ps','-p',str(pid),'-o','time=']).stdout.strip();parts=x.split(':');return float(parts[-1])+int(parts[-2])*60+(int(parts[-3])*3600 if len(parts)>2 else 0)
def enobufs():
 with log.open('rb') as f:f.seek(offset);return f.read().lower().count(b'no buffer space available')
def interrupt(*_):raise RuntimeError('test interrupted; restoring owned changes')
for sig in (signal.SIGINT,signal.SIGTERM):signal.signal(sig,interrupt)
try:
 assert os.geteuid()==0,'administrator required'
 assert IFACE not in run(['/sbin/ifconfig','-l']).stdout.split(),'test interface already exists'
 original={k:sysget(k) for k in keys};original_power=powerget();report['controls_before']={'sysctls':original.copy(),'battery_low_power':original_power}
 if SUITE=='peerpilot':models={'mixed9000':('mixed',9000,'base'),'gvisor9000':('gvisor',9000,'base')};orders=[['mixed9000','gvisor9000']]
 elif SUITE=='tun':models={'gvisor9000':('gvisor',9000,'base'),'gvisor65535':('gvisor',65535,'base'),'mixed9000':('mixed',9000,'base')};orders=[['gvisor9000','gvisor65535','mixed9000'],['mixed9000','gvisor65535','gvisor9000']]
 elif SUITE=='buffer':models={'base':('gvisor',9000,'base'),'buffer':('gvisor',9000,'buffer')};orders=[['base','buffer'],['buffer','base']]
 elif SUITE=='power':models={'base':('gvisor',9000,'base'),'power':('gvisor',9000,'power')};orders=[['base','power'],['power','base']]
 else:raise RuntimeError('unknown suite')
 failed=set()
 with tempfile.TemporaryDirectory(prefix='mole-native-tun-') as td:
  folder=pathlib.Path(td);folder.chmod(0o700)
  for round_no,order in enumerate(orders,1):
   for model in order:
    if model in failed:continue
    stack,mtu,power_profile=models[model];routes=[];p=None;stream=None;check={'model':model,'round':round_no}
    try:
     check['controls']=profile(power_profile);dport=freeport();o=json.loads(json.dumps(base));o['tag']='tested'
     c={'log':{'level':'error'},'dns':{'servers':[{'type':'fakeip','tag':'fake','inet4_range':'198.19.254.0/24'},{'type':'udp','tag':'real','server':'223.5.5.5'}],'rules':[{'query_type':['A'],'server':'fake'}],'final':'real'},'inbounds':[{'type':'tun','tag':'bench-tun','interface_name':IFACE,'address':['172.31.250.1/30'],'auto_route':False,'mtu':mtu,'stack':stack},{'type':'direct','tag':'dns-in','listen':'127.0.0.1','listen_port':dport}],'outbounds':[o],'route':{'default_interface':'en0','rules':[{'action':'sniff'},{'inbound':['dns-in'],'action':'hijack-dns'}],'final':'tested'}}
     path=folder/'config.json';path.write_text(json.dumps(c));path.chmod(0o600);valid=run([ENGINE,'check','-c',str(path)]);assert valid.returncode==0,'configuration invalid: '+safe(valid.stderr)
     logpath=folder/'engine.log';stream=logpath.open('w');p=subprocess.Popen([ENGINE,'run','-c',str(path)],stdout=stream,stderr=subprocess.STDOUT)
     for _ in range(100):
      assert p.poll() is None,'candidate engine exited'
      if IFACE in run(['/sbin/ifconfig','-l']).stdout.split():break
      time.sleep(.03)
     else:raise RuntimeError('candidate interface missing')
     time.sleep(.25)
     if SUITE=='peerpilot':
      peer=run(['/sbin/route','-n','add','-host','172.31.250.2','-interface',IFACE]);assert peer.returncode==0,'owned NAT-peer route rejected'
      routes.append('172.31.250.2')
     addresses={host:dns(dport,host) for host in ('localhost','cloudflare.com')}
     for fake in addresses.values():
      assert fake.startswith('198.19.254.'),'DNS did not create owned FakeIP'
      # Refuse replacing an explicit host route; the existing production /15 is preserved.
      prior=run(['/sbin/route','-n','get',fake]).stdout
      assert 'destination: '+fake+'\n' not in prior,'owned address already has explicit route'
      route=run(['/sbin/route','-n','add','-host',fake,'-interface',IFACE]);assert route.returncode==0,'owned host route rejected'
      routes.append(fake)
     check['warm']=curl(addresses['localhost'],1024);assert check['warm']['ok'],'owned-node pilot payload failed'
     tls=run(['/usr/bin/curl','--silent','--show-error','--noproxy','*','--resolve','cloudflare.com:443:'+addresses['cloudflare.com'],'--max-time','8','https://cloudflare.com/cdn-cgi/trace']);check['tls_authenticated']=tls.returncode==0 and 'ip=' in tls.stdout;assert check['tls_authenticated'],'external authenticated TLS failed'
     for workers,budget in ((1,16<<20),(8,64<<20)):
      sizes=[min(8<<20,budget-i) for i in range(0,budget,8<<20)];c0=cpu(p.pid);t=time.monotonic()
      with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:results=list(pool.map(lambda n:curl(addresses['localhost'],n),sizes))
      elapsed=time.monotonic()-t;cput=cpu(p.pid)-c0;complete=all(x['ok'] for x in results)
      phase={'model':model,'round':round_no,'workers':workers,'budget_bytes':budget,'delivered_bytes':sum(x['bytes'] for x in results),'complete':complete,'elapsed_seconds':elapsed,'aggregate_mbps':sum(x['bytes'] for x in results)*8/elapsed/1e6,'engine_cpu_seconds':cput,'engine_cpu_percent':100*cput/elapsed,'requests':results,'new_production_enobufs':enobufs()};report['phases'].append(phase)
      print(json.dumps({k:phase[k] for k in ('model','round','workers','complete','aggregate_mbps','engine_cpu_percent')}),flush=True)
      assert complete,'phase did not deliver complete budget'
      assert not enobufs() and 'no buffer space available' not in logpath.read_text(),'ENOBUFS safety stop'
      time.sleep(.5)
    except Exception as e:
     check['error']=safe(str(e));failed.add(model)
     print(json.dumps({'model':model,'round':round_no,'error':check['error']}),flush=True)
     if enobufs():raise
    finally:
     for fake in reversed(routes):run(['/sbin/route','-n','delete','-host',fake,'-interface',IFACE])
     if p is not None and p.poll() is None:
      p.terminate()
      try:p.wait(4)
      except subprocess.TimeoutExpired:p.kill();p.wait(4)
     if stream:
      stream.close();check['engine_log_tail']=safe(logpath.read_text(errors='replace'))
     report['checks'].append(check)
     assert not restore(),'could not restore controls'
except Exception as e:report['error']=safe(str(e))
finally:
 try:
  report['restore_errors']=restore();report['controls_after']={'sysctls':{k:sysget(k) for k in keys},'battery_low_power':powerget()}
 except Exception as e:report['restore_error']=str(e)
 report['production_config_after']=hashlib.sha256(CFG.read_bytes()).hexdigest();report['owned_interface_removed']=IFACE not in run(['/sbin/ifconfig','-l']).stdout.split();report['new_production_enobufs']=enobufs();report['finished_at']=datetime.datetime.now().astimezone().isoformat()
 target=ROOT/(SUITE+'.json');target.write_text(json.dumps(report,indent=2)+'\n');target.chmod(0o600);owner=TASK_HOME.stat();os.chown(target,owner.st_uid,owner.st_gid)
 print(json.dumps({'report':str(target),'phases':len(report['phases']),'error':report.get('error'),'restore_errors':report.get('restore_errors'),'owned_interface_removed':report['owned_interface_removed']}),flush=True)
