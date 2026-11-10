import copy,hashlib,json,pathlib,socket,subprocess,tempfile,time,urllib.request
ROOT=pathlib.Path('/tmp/mole-stack-audit-20261103');production=json.loads((pathlib.Path.home()/'.mole/sing-box-config.json').read_text())
r=copy.deepcopy(next(o for o in production['outbounds'] if o.get('type')=='vless'));h=copy.deepcopy(next(o for o in production['outbounds'] if o.get('tag')=='proxy-bw-400'));r['tag']='tested-reality';h['tag']='tested-hy2'
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
results=[]
with tempfile.TemporaryDirectory(prefix='mole-upgrade-api-') as td:
 for label,engine in [('current','/opt/homebrew/bin/sing-box'),('stable',str(ROOT/'v1.14.4/sing-box')),('next',str(ROOT/'v1.15.0-alpha.13/sing-box'))]:
  socks,api=port(),port();c={'log':{'level':'error'},'inbounds':[{'type':'mixed','tag':'test','listen':'127.0.0.1','listen_port':socks}],'outbounds':[{'type':'selector','tag':'proxy','outbounds':['tested-reality','tested-hy2'],'default':'tested-reality'},r,h],'route':{'final':'proxy','default_interface':'en0'},'experimental':{'clash_api':{'external_controller':'127.0.0.1:'+str(api)}}}
  config=pathlib.Path(td)/'config.json';config.write_text(json.dumps(c));config.chmod(0o600);log=pathlib.Path(td)/'engine.log';stream=log.open('w');p=subprocess.Popen([engine,'run','-c',str(config)],stdout=stream,stderr=subprocess.STDOUT);row={'candidate':label,'protocols':[]}
  try:
   for _ in range(100):
    assert p.poll() is None,'candidate process exited'
    try:
     with socket.create_connection(('127.0.0.1',api),.1):break
    except OSError:time.sleep(.03)
   else:raise RuntimeError('candidate API unavailable')
   opener=urllib.request.build_opener(urllib.request.ProxyHandler({}))
   def request(path,body=None):
    q=urllib.request.Request('http://127.0.0.1:'+str(api)+path,data=json.dumps(body).encode() if body else None,method='PUT' if body else 'GET',headers={'Content-Type':'application/json'})
    with opener.open(q,timeout=3) as res:
     value=res.read();return json.loads(value) if value else None
   row['api_version']=request('/version')
   for leg in ('tested-reality','tested-hy2'):
    request('/proxies/proxy',{'name':leg});selected=request('/proxies/proxy')['now'];assert selected==leg,'selector write/read mismatch'
    cp=subprocess.run(['/usr/bin/curl','--silent','--show-error','--noproxy','','--socks5-hostname','127.0.0.1:'+str(socks),'--connect-timeout','5','--max-time','10','--output','/dev/null','--write-out','%{http_code}','https://www.gstatic.com/generate_204'],capture_output=True,text=True)
    tls_ok=cp.returncode==0 and cp.stdout=='204'
    cp2=subprocess.run(['/usr/bin/curl','--silent','--show-error','--noproxy','','--socks5-hostname','127.0.0.1:'+str(socks),'--connect-timeout','5','--max-time','10','--output','/dev/null','--write-out','%{http_code} %{size_download}','http://127.0.0.1:8098/bulk?bytes=8388608'],capture_output=True,text=True)
    fields=cp2.stdout.split();bulk_ok=cp2.returncode==0 and fields==['200','8388608']
    row['protocols'].append({'leg':leg,'selection_matches':selected==leg,'tls_ok':tls_ok,'bulk_8MiB_ok':bulk_ok})
    assert tls_ok and bulk_ok,'authenticated TLS or payload failed'
  except Exception as e:row['error']=str(e)
  finally:
   if p.poll() is None:
    p.terminate()
    try:p.wait(3)
    except subprocess.TimeoutExpired:p.kill();p.wait(3)
   stream.close();row['process_exited']=p.poll() is not None
  results.append(row);print(json.dumps(row),flush=True)
(ROOT/'api-protocol-check.json').write_text(json.dumps(results,indent=2)+'\n')
