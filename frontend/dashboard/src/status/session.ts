import {DemoDataSource} from '../data/demo/demoSource';
import {DEMO_SCENARIOS} from '../data/demo/scenarios';
import {beginDemo,finishDemo,type DemoRecord} from './history';

/** Constructed only by a user's Start action, never Home or a render initializer. */
export class DemoSession extends DemoDataSource {
  private record:DemoRecord;
  private saved=false;
  private finalizing:Promise<void>|null=null;
  private pending:Promise<void>;
  private persist:(r:DemoRecord)=>Promise<void>;
  private begin:(r:DemoRecord)=>Promise<void>;
  private onError:()=>void;
  private transitioning=false;
  private autoAttempted=false;
  constructor(id:string,onError:()=>void,begin=beginDemo,finish=finishDemo){
    if(!DEMO_SCENARIOS.some(s=>s.id===id))throw Error('알 수 없는 시뮬레이션');
    super({initialScenarioId:id,autoLoop:false,autoPlay:true});
    this.record=this.newRecord();this.persist=finish;this.begin=begin;this.onError=onError;this.pending=begin(this.record).catch(onError);
    this.subscribe(()=>{const p=this.getPlayback();if(!this.autoAttempted&&p.simElapsedMs>=p.durationMs){this.autoAttempted=true;void this.finalize('completed').catch(onError);}});
  }
  private newRecord():DemoRecord{const p=this.getPlayback(),s=DEMO_SCENARIOS.find(s=>s.id===p.scenarioId)!;return {id:crypto.randomUUID(),source:'simulation',scenarioId:s.id,title:s.title,engineVersion:1,seed:s.script.seed,startedAt:Date.now(),endedAt:Date.now(),simElapsedMs:0,completion:'interrupted',seekUsed:false};}
  finalize(completion:DemoRecord['completion']):Promise<void>{
    // Every caller must observe the same storage barrier, including its failure.
    if(this.finalizing)return this.finalizing;
    if(this.saved)return Promise.resolve();
    const record={...this.record,completion,endedAt:Date.now(),simElapsedMs:this.getPlayback().simElapsedMs};
    this.finalizing=(async()=>{await this.pending;await this.persist(record);this.saved=true;})()
      .finally(()=>{this.finalizing=null;});
    return this.finalizing;
  }
  override seek(ms:number){this.record.seekUsed=true;super.seek(ms);}
  override select(id:string){if(this.transitioning||!DEMO_SCENARIOS.some(s=>s.id===id))return;this.transitioning=true;this.pause();void this.finalize('stopped').then(()=>{super.select(id);this.record=this.newRecord();this.saved=false;this.autoAttempted=false;this.pending=this.begin(this.record).catch(this.onError);this.play();}).catch(this.onError).finally(()=>{this.transitioning=false;});}
  override restart(){this.select(this.getPlayback().scenarioId);}
  override setAutoLoop(_enabled:boolean){super.setAutoLoop(false);}
}
