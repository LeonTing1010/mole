import contextlib,json,pathlib,socket,struct,subprocess,tempfile,threading,time
engine='/opt/homebrew/bin/sing-box'
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def exact(s,n):
 data=b''
 while len(data)<n:
  b=s.recv(n-len(data))
  if not b:raise RuntimeError('connection closed')
  data+=b
 return data
origin=socket.socket();origin.bind(('127.0.0.1',0));origin.listen();origin.settimeout(.1);stopping=threading.Event()
def serve():
 while not stopping.is_set():
  try:c,_=origin.accept()
  except socket.timeout:continue
  except OSError:return
  with c:
   c.settimeout(2)
   try:
    if exact(c,4)==b'PING':c.sendall(b'PONG')
   except Exception:pass
thread=threading.Thread(target=serve,daemon=True);thread.start()
def application(socks,fake):
 try:
  with socket.create_connection(('127.0.0.1',socks),2) as s:
   s.settimeout(2);s.sendall(b'\x05\x01\x00');assert exact(s,2)==b'\x05\x00'
   s.sendall(b'\x05\x01\x00\x01'+socket.inet_aton(fake)+struct.pack('!H',origin.getsockname()[1]))
   reply=exact(s,4)
   if reply[1]!=0:return {'ok':False,'socks_status':reply[1]}
   if reply[3]==1:exact(s,6)
   elif reply[3]==4:exact(s,18)
   else:exact(s,exact(s,1)[0]+2)
   s.sendall(b'PING');body=exact(s,4)
   return {'ok':body==b'PONG','body':body.decode(errors='replace')}
 except Exception as e:return {'ok':False,'error':str(e)}
report={'scope':'isolated sing-box 1.14.3; production-style sniff enabled; plain non-SNI TCP; graceful engine restart; production unchanged','cases':[]}
try:
 with tempfile.TemporaryDirectory(prefix='mole-fakeip-audit-') as td:
  root=pathlib.Path(td)
  for persistent in (False,True):
   socks,dns=port(),port();cfg={'log':{'level':'error'},'dns':{'servers':[{'type':'fakeip','tag':'fake','inet4_range':'198.18.0.0/15'},{'type':'hosts','tag':'hosts','predefined':{'cache-probe.example':['127.0.0.1']}}],'rules':[{'query_type':['A'],'server':'fake'}],'final':'hosts'},'inbounds':[{'type':'mixed','tag':'app','listen':'127.0.0.1','listen_port':socks},{'type':'direct','tag':'dns-in','listen':'127.0.0.1','listen_port':dns}],'outbounds':[{'type':'direct','tag':'direct','domain_resolver':{'server':'hosts'}}],'route':{'rules':[{'action':'sniff'},{'inbound':['dns-in'],'action':'hijack-dns'}],'final':'direct'}}
   if persistent:cfg['experimental']={'cache_file':{'enabled':True,'path':str(root/'cache.db'),'store_fakeip':True}}
   path=root/('persistent.json' if persistent else 'volatile.json');path.write_text(json.dumps(cfg));path.chmod(0o600)
   cp=subprocess.run([engine,'check','-c',str(path)],capture_output=True,text=True)
   case={'persistent':persistent,'config_valid':cp.returncode==0};report['cases'].append(case)
   if cp.returncode:case['check_error']=cp.stderr[-1200:];continue
   logpath=path.with_suffix('.log')
   def launch():
    stream=logpath.open('a');p=subprocess.Popen([engine,'run','-c',str(path)],stdout=stream,stderr=subprocess.STDOUT)
    for _ in range(100):
     if p.poll() is not None:raise RuntimeError('engine exited: '+logpath.read_text()[-1000:])
     try:
      with socket.create_connection(('127.0.0.1',socks),.1):return p,stream
     except OSError:time.sleep(.02)
    raise RuntimeError('engine start timeout')
   def stop(p,stream):
    if p.poll() is None:
     p.terminate()
     try:p.wait(3)
     except subprocess.TimeoutExpired:p.kill();p.wait(3)
    stream.close()
   p,stream=launch()
   try:
    qname=b''.join(bytes([len(x)])+x.encode() for x in 'cache-probe.example'.split('.'))+b'\x00'
    query=struct.pack('!HHHHHH',12345,0x100,1,0,0,0)+qname+struct.pack('!HH',1,1)
    with socket.socket(socket.AF_INET,socket.SOCK_DGRAM) as s:
     s.settimeout(2);s.sendto(query,('127.0.0.1',dns));reply=s.recv(4096);assert struct.unpack('!H',reply[6:8])[0]==1;fake=socket.inet_ntoa(reply[-4:])
    case['cached_fakeip']=fake;case['before_restart']=application(socks,fake)
   finally:stop(p,stream)
   p,stream=launch()
   try:case['after_restart_without_dns_refresh']=application(socks,fake)
   finally:stop(p,stream)
   case['log_tail']=logpath.read_text(errors='replace')[-1000:]
except Exception as e:report['error']=str(e)
finally:stopping.set();origin.close();thread.join(1)
p=pathlib.Path('/tmp/mole-fakeip-restart-sniff-audit-20261103.json');p.write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
