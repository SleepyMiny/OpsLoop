import {parseStatus, parseStrictJSON, statusState, type StatusEnvelope} from './contract';
export interface StatusView {envelope: StatusEnvelope | null; error: string | null; loading: boolean; now: number}
export function parseEnvelope(v: unknown): StatusEnvelope {
  if (!v || typeof v !== 'object' || Array.isArray(v)) throw Error('잘못된 공개 응답');
  const x=v as Record<string,unknown>;
  if (Object.keys(x).sort().join()!==['schemaVersion','serverTime','receivedAt','dataState','health','snapshot'].sort().join() || x.schemaVersion!==1 || !Number.isSafeInteger(x.serverTime) || !Number.isSafeInteger(x.receivedAt) || Number(x.serverTime)<=0 || Number(x.receivedAt)<=0 || Number(x.receivedAt)>Number(x.serverTime)+5000) throw Error('잘못된 공개 응답');
  const snapshot=parseStatus(x.snapshot), state=statusState(snapshot,Number(x.serverTime));
  if (x.dataState!==state.dataState || x.health!==state.health) throw Error('관측 상태 불일치');
  return {...x,snapshot} as unknown as StatusEnvelope;
}
export class StatusClient {
  private running=false;
  private generation=0;
  private timer:ReturnType<typeof setTimeout>|undefined;
  private tick:ReturnType<typeof setInterval>|undefined;
  private controller:AbortController|undefined;
  private mono=0;
  private server=0;
  private listeners=new Set<()=>void>();
  private view:StatusView={envelope:null,error:null,loading:true,now:Date.now()};
  constructor(private fetcher:typeof fetch=(...args)=>globalThis.fetch(...args),private monotonic=()=>performance.now(),private pollMs=30000){}
  subscribe=(fn:()=>void)=>{this.listeners.add(fn);return()=>{this.listeners.delete(fn);};};
  getSnapshot=()=>this.view;
  private emit(){for(const fn of this.listeners)fn();}
  private clock(){return this.server ? this.server+this.monotonic()-this.mono : Date.now();}
  start=()=>{if(this.running)return;this.running=true;this.generation++;void this.poll(this.generation);this.tick=setInterval(()=>{this.view={...this.view,now:this.clock()};this.emit();},1000);};
  stop=()=>{this.running=false;this.generation++;clearTimeout(this.timer);clearInterval(this.tick);this.controller?.abort();};
  private async poll(g:number){
    if(!this.running||g!==this.generation)return;
    const c=new AbortController();this.controller=c;const timeout=setTimeout(()=>c.abort(),4000);
    try{
      const r=await this.fetcher('/api/status',{method:'GET',credentials:'omit',redirect:'error',cache:'no-store',headers:{Accept:'application/json'},signal:c.signal});
      if(!r.ok)throw Error(`관측 API HTTP ${r.status}`);
      if(!r.headers.get('Content-Type')?.startsWith('application/json'))throw Error('관측 API JSON 응답이 아닙니다');
      if(!r.body)throw Error('관측 API 응답 없음');const reader=r.body.getReader();let size=0;const chunks:Uint8Array[]=[];
      try{for(;;){const {value,done}=await reader.read();if(done)break;size+=value.length;if(size>16384)throw Error('관측 API 응답 크기 초과');chunks.push(value);}}finally{await reader.cancel().catch(()=>{});}
      const raw=new Uint8Array(size);let at=0;for(const x of chunks){raw.set(x,at);at+=x.length;}
      const envelope=parseEnvelope(parseStrictJSON(new TextDecoder('utf-8',{fatal:true}).decode(raw)));
      if(!this.running||g!==this.generation)return;
      this.server=envelope.serverTime;this.mono=this.monotonic();this.view={envelope,error:null,loading:false,now:this.server};
    }catch{if(!this.running||g!==this.generation)return;this.view={...this.view,error:'현재 관측 API에 연결할 수 없습니다. 이전 값은 마지막 관측이며 현재 정상 상태를 뜻하지 않습니다.',loading:false,now:this.clock()};}
    finally{clearTimeout(timeout);}
    this.emit();if(this.running&&g===this.generation)this.timer=setTimeout(()=>void this.poll(g),this.view.error?Math.min(this.pollMs*2,120000):this.pollMs);
  }
}
