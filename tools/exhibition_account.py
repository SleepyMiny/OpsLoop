"""Finite account/limits read through the pinned Codex protocol, never a turn.

Called only by a separately approved execution session. No login, token refresh
request, credit consumption, model turn, thread creation or paid API fallback.
Only a sanitized limits receipt leaves the temporary private auth directory.
"""
import json,os,selectors,signal,subprocess,tempfile,time,hashlib,stat
from pathlib import Path
from tools.exhibition_hashes import require


def check_result(account,limits,*,observed_at):
    require(account.get('account',{}).get('type')=='chatgpt','CHATGPT_ACCOUNT_REQUIRED')
    buckets=limits.get('rateLimitsByLimitId') or {'codex':limits.get('rateLimits')}
    require(isinstance(buckets,dict) and buckets,'ACCOUNT_LIMITS_UNCONFIRMED');out={}
    for name,b in buckets.items():
        require(isinstance(b,dict) and not b.get('rateLimitReachedType'),'ACCOUNT_LIMIT_REACHED_OR_UNKNOWN')
        windows={}
        for w in ('primary','secondary'):
            v=b.get(w)
            if v is None:continue
            used=v.get('usedPercent');reset=v.get('resetsAt')
            require(type(used) in (int,float) and 0<=used<100 and type(reset) is int and reset>observed_at,'ACCOUNT_WINDOW_UNAVAILABLE')
            windows[w]={'usedPercent':used,'resetsAt':reset,'windowDurationMins':v.get('windowDurationMins')}
        require(windows,'ACCOUNT_WINDOW_MISSING');out[name]=windows
    return {'schema':'opsloop-chatgpt-availability/v1','authMode':'chatgpt','observedAt':observed_at,'limits':out,
        'state':'NOT_CURRENTLY_RATE_LIMITED','reservedCalls':False,'modelCalls':0,'readOnlyProtocolMethods':['account/read','account/rateLimits/read']}


def read_once(cfg,*,authorize):
    authorize();binary=Path(cfg['codex_binary']);auth=Path(cfg['auth_file'])
    require(hashlib.sha256(binary.read_bytes()).hexdigest()==cfg['codex_binary_sha256'],'CODEX_BINARY_CHANGED')
    st=auth.stat(follow_symlinks=False);require(not auth.is_symlink() and stat.S_ISREG(st.st_mode) and st.st_uid==os.getuid() and not st.st_mode&0o077,'PRIVATE_CHATGPT_AUTH_REQUIRED')
    raw=auth.read_bytes();document=json.loads(raw)
    require(document.get('auth_mode')=='chatgpt' and document.get('OPENAI_API_KEY') in (None,''),'CHATGPT_AUTH_ONLY')
    with tempfile.TemporaryDirectory(prefix='opsloop-account-read-') as directory:
        root=Path(directory);fd=os.open(root/'auth.json',os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
        with os.fdopen(fd,'wb') as f:f.write(raw)
        del raw,document
        env={'PATH':'/usr/bin:/bin','HOME':directory,'CODEX_HOME':directory,'LANG':'C','RUST_LOG':'off'}
        p=subprocess.Popen([str(binary),'app-server','--stdio','-c','analytics.enabled=false','-c','feedback.enabled=false'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,cwd=directory,env=env,start_new_session=True)
        end=time.monotonic()+25;buffer=b'';sel=selectors.DefaultSelector();sel.register(p.stdout,selectors.EVENT_READ)
        def send(v):authorize();p.stdin.write(json.dumps(v,separators=(',',':')).encode()+b'\n');p.stdin.flush()
        def rpc(index,method,params=None):
            nonlocal buffer
            msg={'id':index,'method':method}
            if params is not None:msg['params']=params
            send(msg)
            while time.monotonic()<end:
                authorize()
                if b'\n' not in buffer:
                    if not sel.select(min(.2,max(0,end-time.monotonic()))):continue
                    chunk=os.read(p.stdout.fileno(),65536);require(chunk,'ACCOUNT_PROCESS_ENDED');buffer+=chunk
                    require(len(buffer)<=1048576,'ACCOUNT_RESPONSE_LIMIT')
                    continue
                line,buffer=buffer.split(b'\n',1);v=json.loads(line)
                # Never supply refreshed tokens or perform requested side effects.
                require(not ('method' in v and 'id' in v),'ACCOUNT_UNEXPECTED_SERVER_REQUEST')
                if v.get('id')==index:
                    require('error' not in v and isinstance(v.get('result'),dict),'ACCOUNT_READ_FAILED');return v['result']
            raise ValueError('ACCOUNT_READ_TIMEOUT')
        try:
            rpc(1,'initialize',{'clientInfo':{'name':'opsloop-limits-read','version':'1'},'capabilities':{'experimentalApi':False}})
            send({'method':'initialized'})
            account=rpc(2,'account/read',{'refreshToken':False});limits=rpc(3,'account/rateLimits/read')
            return check_result(account,limits,observed_at=time.time())
        finally:
            sel.close()
            if p.poll() is None:os.killpg(p.pid,signal.SIGTERM)
            try:p.wait(timeout=2)
            except subprocess.TimeoutExpired:os.killpg(p.pid,signal.SIGKILL);p.wait(timeout=2)
