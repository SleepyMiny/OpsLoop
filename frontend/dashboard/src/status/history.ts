export interface DemoRecord {id:string;source:'simulation';scenarioId:string;title:string;engineVersion:1;seed:number;startedAt:number;endedAt:number;simElapsedMs:number;completion:'completed'|'stopped'|'interrupted'|'unfinished';seekUsed:boolean}
const DB='opsloop-simulation-history-v1', LIMIT=50, TTL=30*86400000;
function open():Promise<IDBDatabase>{return new Promise((resolve,reject)=>{const r=indexedDB.open(DB,1);r.onupgradeneeded=()=>{r.result.createObjectStore('history',{keyPath:'id'});r.result.createObjectStore('pending',{keyPath:'id'});};r.onsuccess=()=>resolve(r.result);r.onerror=()=>reject(Error('시뮬레이션 기록 저장소를 열 수 없습니다'));r.onblocked=()=>reject(Error('다른 탭이 기록 저장소를 사용 중입니다'));});}
async function transaction(stores:string[],run:(t:IDBTransaction)=>void):Promise<void>{const db=await open();try{await new Promise<void>((resolve,reject)=>{const t=db.transaction(stores,'readwrite');t.oncomplete=()=>resolve();t.onerror=()=>reject(Error('시뮬레이션 기록 저장 실패'));t.onabort=()=>reject(Error('시뮬레이션 기록 저장 실패'));run(t);});}finally{db.close();}}
function valid(x:unknown):x is DemoRecord {if(!x||typeof x!=='object')return false;const r=x as DemoRecord;return r.source==='simulation'&&r.engineVersion===1&&typeof r.id==='string'&&/^[a-f0-9-]{36}$/.test(r.id)&&typeof r.title==='string'&&r.title.length<=100&&typeof r.scenarioId==='string'&&/^[a-zA-Z0-9_-]{1,80}$/.test(r.scenarioId)&&Number.isSafeInteger(r.seed)&&Number.isSafeInteger(r.startedAt)&&Number.isSafeInteger(r.endedAt)&&r.endedAt>=r.startedAt&&Number.isFinite(r.simElapsedMs)&&r.simElapsedMs>=0&&typeof r.seekUsed==='boolean'&&['completed','stopped','interrupted','unfinished'].includes(r.completion);}
export const beginDemo=(r:DemoRecord)=>transaction(['pending'],t=>{t.objectStore('pending').put({...r,completion:'unfinished'});});
export const finishDemo=async(r:DemoRecord)=>{if(!valid(r))throw Error('잘못된 시뮬레이션 기록');await transaction(['pending','history'],t=>{t.objectStore('pending').delete(r.id);t.objectStore('history').put(r);});await readHistory();};
export async function readHistory():Promise<DemoRecord[]>{
  const db=await open();
  try{return await new Promise<DemoRecord[]>((resolve,reject)=>{
    const t=db.transaction(['history','pending'],'readwrite'),rows:Array<{record:DemoRecord;key:IDBValidKey;store:string}>=[];let remaining=2;let kept:DemoRecord[]=[];
    t.onerror=()=>reject(Error('시뮬레이션 기록 조회 실패'));t.onabort=()=>reject(Error('시뮬레이션 기록 조회 실패'));t.oncomplete=()=>resolve(kept);
    for(const name of ['history','pending']){const r=t.objectStore(name).openCursor();r.onsuccess=()=>{const cursor=r.result;if(cursor){if(!valid(cursor.value)||cursor.value.startedAt<Date.now()-TTL)cursor.delete();else rows.push({record:cursor.value,key:cursor.key,store:name});cursor.continue();return;}
      if(--remaining===0){rows.sort((a,b)=>b.record.endedAt-a.record.endedAt);const seen=new Set<string>();for(const row of rows){if(kept.length>=LIMIT||seen.has(row.record.id))t.objectStore(row.store).delete(row.key);else{seen.add(row.record.id);kept.push(row.record);}}}
    };}
  });}finally{db.close();}
}
export const clearHistory=()=>transaction(['history','pending'],t=>{t.objectStore('history').clear();t.objectStore('pending').clear();});
