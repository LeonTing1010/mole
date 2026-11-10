import datetime,hashlib,json,pathlib,stat,subprocess,tempfile
root=pathlib.Path.home()/'.mole';log=root/'mole.log';offset=log.stat().st_size
report={'started_at':datetime.datetime.now().astimezone().isoformat(),'checks':[]}
def trace_ip(body):
 for line in body.decode(errors='replace').splitlines():
  if line.startswith('ip='):return line[3:]
 return ''
fixed=subprocess.run(['/usr/bin/curl','--silent','--show-error','--noproxy','*','--connect-to','cloudflare.com:443:127.0.0.1:8100','--connect-timeout','5','--max-time','8','https://cloudflare.com/cdn-cgi/trace'],capture_output=True)
exit_ip=trace_ip(fixed.stdout);assert fixed.returncode==0 and exit_ip,'named REALITY external HTTPS exit observation failed'
report['authenticated_named_reality_exit']=exit_ip
with tempfile.TemporaryDirectory(prefix='mole-tun-https-') as td:
 for i in range(10):
  for host,path,status,body in [('cloudflare.com','/cdn-cgi/trace',200,True),('www.gstatic.com','/generate_204',204,False)]:
   target=pathlib.Path(td)/'body'
   cp=subprocess.run(['/usr/bin/curl','--silent','--show-error','--noproxy','*','--connect-timeout','5','--max-time','8','--output',str(target),'--write-out','%{http_code} %{time_total}','https://'+host+path],capture_output=True,text=True)
   fields=cp.stdout.split();code=int(fields[0]) if fields and fields[0].isdigit() else 0;payload=target.read_bytes() if target.exists() else b''
   result={'host':host,'trial':i+1,'status':code,'seconds':float(fields[1]) if len(fields)>1 else None,'body_bytes':len(payload),'ok':cp.returncode==0 and code==status and (len(payload)>0 if body else len(payload)==0),'error':cp.stderr[-300:]}
   if host=='cloudflare.com':result['matches_authenticated_node_exit']=trace_ip(payload)==exit_ip;result['ok']=result['ok'] and result['matches_authenticated_node_exit']
   report['checks'].append(result)
state=json.loads((root/'state.json').read_text());report['state']={k:state.get(k) for k in ('pid','singbox_pid','mode','lid_destroy','reality_pacing_state','reality_rate_mbps','last_keepalive_ms','last_keepalive_error','last_dial_error','dial_samples')};report['state']['keepalive_fails']=state.get('keepalive_fails',0)
c=json.loads((root/'sing-box-config.json').read_text());report['cache_config']=c['experimental'].get('cache_file');cache=root/'sing-box-cache.db';report['cache_file_exists']=cache.exists();report['cache_file_mode']=oct(stat.S_IMODE(cache.stat().st_mode)) if cache.exists() else None;report['cache_file_bytes']=cache.stat().st_size if cache.exists() else None
report['smux']=[o.get('multiplex') for o in c['outbounds'] if o.get('type')=='vless'];report['tun_stack']=[o.get('stack') for o in c['inbounds'] if o.get('type')=='tun'];report['selector']=json.loads(subprocess.check_output(['/usr/bin/curl','--silent','--noproxy','*','http://127.0.0.1:9090/proxies/proxy'],text=True))['now'];report['installed_sha256']=hashlib.sha256(pathlib.Path('/usr/local/bin/mole').read_bytes()).hexdigest()
with log.open('rb') as f:f.seek(offset);lines=f.read().decode(errors='replace')
report['new_error_counts']={name:lines.lower().count(pattern) for name,pattern in [('enobufs','no buffer space available'),('dial_timeout','i/o timeout'),('missing_fakeip','missing fakeip record')]};report['finished_at']=datetime.datetime.now().astimezone().isoformat()
p=pathlib.Path('/tmp/mole-network-fix-postactivation-final-20261103.json');p.write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps({'success':sum(c['ok'] for c in report['checks']),'total':len(report['checks']),'pid':state['pid'],'selector':report['selector'],'cache_mode':report['cache_file_mode'],'errors':report['new_error_counts']}))
