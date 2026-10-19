import evidence from './evidence.json';
import { ReplaySource } from './replayCore';
export { parseReplay } from './replayCore';

/** Local Live/Replay entry keeps its existing sanitized fixture and default. */
export class ReplayDataSource extends ReplaySource {
  constructor(input: unknown = evidence) { super(input); }
}
