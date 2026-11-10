import copy, concurrent.futures, datetime, importlib.util, json, pathlib, socket, subprocess, tempfile, time
repo=pathlib.Path(__file__).resolve().parents[3]
spec=importlib.util.spec_from_file_location('measure',repo/'scripts/measure-dual.py'); measure=importlib.util.module_from_spec(spec);spec.loader.exec_module(measure)
config=json.loads((pathlib.Path.home()/'.mole/sing-box-config.json').read_text())
base=next(o for o in config['outbounds'] if o.get('type')=='vless')
engine='/opt/homebrew/bin/sing-box'
def safe(s):
    for value in (base.get('server',''),base.get('uuid',''),base.get('tls',{}).get('reality',{}).get('public_key','')):
        if value:s=s.replace(value,'<redacted>')
    return s[-600:]
def port():
    with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
report={'started_at':datetime.datetime.now().astimezone().isoformat(),'scope':'Current Wi-Fi; isolated same-node clients; production unchanged; no upload, server/network-fault recovery, long-term or wired validation','phases':[],'https':[],'checks':{}}
procs={};streams={};ports={};files={}
def launch(mode):
    stream=files[mode].with_suffix('.log').open('a');streams[mode]=stream
    p=subprocess.Popen([engine,'run','-c',str(files[mode])],stdout=stream,stderr=subprocess.STDOUT);procs[mode]=p
    for _ in range(100):
        if p.poll() is not None:raise RuntimeError('test engine exited')
        try:
            with socket.create_connection(('127.0.0.1',ports[mode]['socks']),timeout=.1):return
        except OSError:time.sleep(.02)
    raise RuntimeError('test listener not ready')
def stop(mode):
    p=procs[mode]
    if p.poll() is None:
        p.terminate()
        try:p.wait(timeout=3)
        except subprocess.TimeoutExpired:p.kill();p.wait(timeout=3)
    streams[mode].close()
def https(mode,stage):
    cp=subprocess.run(['curl','--silent','--show-error','--noproxy','','--socks5-hostname','127.0.0.1:'+str(ports[mode]['socks']),'--connect-timeout','5','--max-time','10','--output','/dev/null','--write-out','%{http_code} %{time_total}','https://www.gstatic.com/generate_204'],capture_output=True,text=True)
    fields=cp.stdout.split();code=int(fields[0]) if fields and fields[0].isdigit() else 0
    r={'mode':mode,'stage':stage,'ok':cp.returncode==0 and code==204,'status':code,'seconds':float(fields[1]) if len(fields)>1 else None,'error':safe(cp.stderr)};report['https'].append(r);return r['ok']
with tempfile.TemporaryDirectory(prefix='mole-native-ab-') as td:
    root=pathlib.Path(td)
    try:
        for mode in ('plain','smux'):
            ports[mode]={'socks':port(),'bulk':port()}
            o=copy.deepcopy(base);o['tag']='tested'
            if mode=='smux':o['multiplex']={'enabled':True,'protocol':'smux','max_connections':8,'min_streams':1}
            else:o.pop('multiplex',None)
            c={'log':{'level':'error'},'inbounds':[{'type':'mixed','tag':'https','listen':'127.0.0.1','listen_port':ports[mode]['socks']},{'type':'direct','tag':'bulk','listen':'127.0.0.1','listen_port':ports[mode]['bulk'],'override_address':'127.0.0.1','override_port':8098}],'outbounds':[o],'route':{'final':'tested'}}
            f=root/(mode+'.json');f.write_text(json.dumps(c));f.chmod(0o600);files[mode]=f
            check=subprocess.run([engine,'check','-c',str(f)],capture_output=True,text=True)
            report['checks'][mode]={'valid':check.returncode==0,'error':safe(check.stderr)}
            if check.returncode:raise RuntimeError('engine configuration rejected')
            launch(mode)
        for mode in ('plain','smux'):https(mode,'cold')
        watch=measure.OverflowWatch()
        for round_no,order in enumerate((('plain','smux'),('smux','plain')),1):
            for workers,budget in ((1,16<<20),(8,64<<20)):
                for mode in order:
                    phase=measure.run_phase(mode,[ports[mode]['bulk']],15,budget,workers,watch)
                    phase['round']=round_no;phase['workers']=workers
                    phase['legs']={'tested':next(iter(phase['legs'].values()))}
                    report['phases'].append(phase)
                    print(json.dumps({'mode':mode,'round':round_no,'workers':workers,'complete':phase['complete'],'mbps':round(phase['aggregate_mbps'],2),'stop_reason':phase['stop_reason']}),flush=True)
                    https(mode,'warm-after-bulk')
                    if phase['new_enobufs_lines']:raise RuntimeError('ENOBUFS safety stop')
                    time.sleep(2)
        for mode in ('plain','smux'):
            stop(mode);launch(mode);https(mode,'client-restart-only')
    except Exception as exc:report['error']=safe(str(exc))
    finally:
        for mode in list(procs):
            stop(mode)
            if files[mode].with_suffix('.log').exists():report['checks'][mode]['log_tail']=safe(files[mode].with_suffix('.log').read_text(errors='replace'))
report['all_test_processes_exited']=all(p.poll() is not None for p in procs.values());report['finished_at']=datetime.datetime.now().astimezone().isoformat()
out=pathlib.Path('/tmp/mole-native-ab-20261029.json');out.write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps({'output':str(out),'all_test_processes_exited':report['all_test_processes_exited'],'https_success':sum(r['ok'] for r in report['https']),'https_total':len(report['https']),'error':report.get('error')}))
