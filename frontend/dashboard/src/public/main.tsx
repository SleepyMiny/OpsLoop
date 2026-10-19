import '@fontsource/barlow/500.css';
import '@fontsource/barlow/600.css';
import '@fontsource/barlow/700.css';
import '@fontsource/ibm-plex-sans-kr/400.css';
import '@fontsource/ibm-plex-sans-kr/500.css';
import '@fontsource/ibm-plex-sans-kr/600.css';
import '../styles/tokens.css';
import '../styles/base.css';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from '../app/App';
import { createPublicDataSource, UnsupportedPublicMode } from './createPublicDataSource';

const root = document.getElementById('root');
if (!root) throw new Error('#root missing');
function PublicApp() {
  try {
    return <App source={createPublicDataSource(window.location.search)} kiosk={false} publicView />;
  } catch (error) {
    if (!(error instanceof UnsupportedPublicMode)) throw error;
    return <main style={{maxWidth: '44rem', margin: '4rem auto', padding: '1.5rem'}}>
      <h1>OpsLoop 공개 전시</h1>
      <h2>지원하지 않는 공개 모드</h2>
      <p>{error.message}</p>
      <p>요청한 출처를 다른 데이터로 자동 대체하지 않았습니다. API 요청도 보내지 않습니다.</p>
      <nav aria-label="공개 데이터 모드"><a href="?source=demo">Demo · 시뮬레이션</a>{' · '}<a href="?source=replay">Replay · 과거 ObserveOnly 실증</a></nav>
    </main>;
  }
}
createRoot(root).render(<StrictMode><PublicApp /></StrictMode>);
