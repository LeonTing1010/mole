"""Bounded, isolated full-TUN stack comparison; never changes system default route."""
import concurrent.futures,copy,datetime,hashlib,json,os,pathlib,signal,socket,struct,subprocess,tempfile,time
CONFIG=pathlib.Path('/Users/leo/.mole/sing-box-config.json')
OUTPUT=pathlib.Path('/tmp/mole-tun-ab-20261103.json')
ENGINE='/opt/homebrew/bin/sing-box'
IFACE='utun30'
owner=CONFIG.parent.parent.stat()
base_config=json.loads(CONFIG.read_text())
base=next(o for o in base_config['outbounds'] if o.get('type')=='vless')
before=hashlib.sha256(CONFIG.read_bytes()).hexdigest()
report={'scope':'isolated auto_route:false full TUN; interface-scoped default only; en0 transport; same node REALITY+smux; 1/8 equal flows, bounded owned-node download; production daemon remains active', 'started_at':datetime.datetime.now().astimezone().isoformat(),'phases':[],'checks':{},'production_config_before':before}

def clean_error(s):
 for v in (base.get('server',''),base.get('uuid',''),base.get('tls',{}).get('reality',{}).get('public_key','')):
  if v:s=s.replace(v,'<redacted>')
 return s[-4000:]
def run(args,**kw):return subprocess.run(args,capture_output=True,text=True,**kw)
def freeport():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def dns_query(port):
 q=struct.pack('!HHHHHH',21531,0x100,1,0,0,0)+b'\x09localhost\0'+struct.pack('!HH',1,1)
 with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as s:
  s.settimeout(3);s.sendto(q,('127.0.0.1',port));r=s.recv(4096)
  assert struct.unpack('!H',r[6:8])[0]==1,'expected one native FakeIP mapping'
  return socket.inet_ntoa(r[-4:])
def curl(fake,size):
 cp=run(['/usr/bin/curl','--silent','--show-error','--noproxy','*','--interface',IFACE,'--resolve','localhost:8098:'+fake,'--connect-timeout','5','--max-time','20','--output','/dev/null','--write-out','%{http_code} %{size_download} %{time_total}','http://localhost:8098/bulk?bytes='+str(size)])
 fields=cp.stdout.split();code=int(fields[0]) if fields and fields[0].isdigit() else 0
 received=int(fields[1]) if len(fields)>1 else 0
 return {'ok':cp.returncode==0 and code==200 and received==size,'status':code,'bytes':received,'wanted':size,'seconds':float(fields[2]) if len(fields)>2 else None,'error':clean_error(cp.stderr)}
def interrupted(*_):raise RuntimeError('test interrupted; cleaning owned scoped route')
for sig in (signal.SIGTERM,signal.SIGINT):signal.signal(sig,interrupted)
try:
 if os.geteuid()!=0:raise RuntimeError('administrator authentication required for isolated TUN')
 if IFACE in run(['/sbin/ifconfig','-l']).stdout.split():raise RuntimeError('reserved test interface already exists; refusing to alter it')
 with tempfile.TemporaryDirectory(prefix='mole-tun-ab-') as td:
  root=pathlib.Path(td);root.chmod(0o700)
  for round_no,order in enumerate((('mixed','gvisor'),('gvisor','mixed')),1):
   for stack in order:
    proc=None;scoped=False;stream=None
    try:
     dns=freeport();o=copy.deepcopy(base);o['tag']='tested'
     c={'log':{'level':'debug'},'dns':{'servers':[{'type':'fakeip','tag':'fake','inet4_range':'198.18.0.0/15'},{'type':'udp','tag':'real','server':'223.5.5.5'}],'rules':[{'query_type':['A'],'server':'fake'}],'final':'real'},'inbounds':[{'type':'tun','tag':'bench-tun','interface_name':IFACE,'address':['172.31.250.1/30'],'auto_route':False,'mtu':9000,'stack':stack},{'type':'direct','tag':'bench-dns','listen':'127.0.0.1','listen_port':dns}],'outbounds':[o],'route':{'default_interface':'en0','rules':[{'action':'sniff'},{'inbound':['bench-dns'],'action':'hijack-dns'}],'final':'tested'}}
     path=root/(stack+'.json');path.write_text(json.dumps(c));path.chmod(0o600)
     check=run([ENGINE,'check','-c',str(path)]);report['checks'][stack]={'config_valid':check.returncode==0,'check_error':clean_error(check.stderr)}
     if check.returncode:raise RuntimeError('candidate engine config invalid: '+clean_error(check.stderr))
     logpath=root/(stack+'.log');stream=logpath.open('a');proc=subprocess.Popen([ENGINE,'run','-c',str(path)],stdout=stream,stderr=subprocess.STDOUT)
     for _ in range(100):
      if proc.poll() is not None:raise RuntimeError('test engine exited: '+logpath.read_text()[-600:])
      if IFACE in run(['/sbin/ifconfig','-l']).stdout.split():break
      time.sleep(.05)
     else:raise RuntimeError('owned TUN did not start')
     route=run(['/sbin/route','-n','add','-ifscope',IFACE,'default','-interface',IFACE]);scoped=route.returncode==0
     if not scoped:raise RuntimeError('interface-scoped test route failed: '+route.stderr)
     time.sleep(1)
     fake=dns_query(dns)
     warm=curl(fake,1024);report['checks'][stack]['warm_payload']=warm
     if not warm['ok']:
      time.sleep(1);retry=curl(fake,1024);report['checks'][stack]['warm_retry']=retry
      if not retry['ok']:raise RuntimeError('full-TUN pilot failed twice after listener settle')
     for workers,budget in ((1,16<<20),(8,64<<20)):
      sizes=[min(8<<20,budget-i) for i in range(0,budget,8<<20)]
      began=time.monotonic()
      with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:results=list(pool.map(lambda n:curl(fake,n),sizes))
      elapsed=time.monotonic()-began;complete=all(x['ok'] for x in results)
      phase={'stack':stack,'round':round_no,'workers':workers,'budget_bytes':budget,'delivered_bytes':sum(x['bytes'] for x in results),'elapsed_seconds':elapsed,'complete':complete,'aggregate_mbps':sum(x['bytes'] for x in results)*8/elapsed/1e6,'requests':results,'engine_cpu_snapshot':run(['/bin/ps','-p',str(proc.pid),'-o','%cpu=']).stdout.strip()}
      report['phases'].append(phase)
      print(json.dumps({k:phase[k] for k in ('stack','round','workers','complete','aggregate_mbps')}),flush=True)
      if not complete:raise RuntimeError('incomplete full-TUN payload; no capacity conclusion')
      if 'no buffer space available' in logpath.read_text():raise RuntimeError('ENOBUFS safety stop')
      time.sleep(1)
    finally:
     if scoped:run(['/sbin/route','-n','delete','-ifscope',IFACE,'default','-interface',IFACE])
     if proc is not None and proc.poll() is None:
      proc.terminate()
      try:proc.wait(4)
      except subprocess.TimeoutExpired:proc.kill();proc.wait(4)
     if stream:
      stream.close()
      report['checks'][stack]['log_tail']=clean_error(logpath.read_text(errors='replace')[-4000:])
except Exception as e:report['error']=clean_error(str(e))
finally:
 report['production_config_after']=hashlib.sha256(CONFIG.read_bytes()).hexdigest()
 report['test_interface_removed']=IFACE not in run(['/sbin/ifconfig','-l']).stdout.split()
 report['finished_at']=datetime.datetime.now().astimezone().isoformat()
 OUTPUT.write_text(json.dumps(report,indent=2)+'\n');os.chmod(OUTPUT,0o600)
 if os.geteuid()==0:os.chown(OUTPUT,owner.st_uid,owner.st_gid)
 print(json.dumps({'report':str(OUTPUT),'phases':len(report['phases']),'error':report.get('error'),'test_interface_removed':report['test_interface_removed']}),flush=True)
