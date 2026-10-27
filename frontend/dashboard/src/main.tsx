import '@fontsource/barlow/500.css';
import '@fontsource/barlow/600.css';
import '@fontsource/barlow/700.css';
import '@fontsource/ibm-plex-sans-kr/400.css';
import '@fontsource/ibm-plex-sans-kr/500.css';
import '@fontsource/ibm-plex-sans-kr/600.css';
import './styles/tokens.css';
import './styles/base.css';

import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './app/App';
import { createDataSource } from './data/createDataSource';
import { M3App } from './m3/M3App';

const params = new URLSearchParams(window.location.search);
// Synthetic scenarios remain available only via an explicit developer route.
// Exhibition and Replay use the hash-verified Demo A/B catalog without live I/O.
const legacy = params.get('source') === 'simulation' || params.get('source') === 'legacy-replay';
const source = legacy ? createDataSource({ search: params.get('source') === 'simulation' ? window.location.search.replace('source=simulation', 'source=demo') : '?source=replay', env: import.meta.env }) : null;
const kiosk = params.get('kiosk') === '1';

const root = document.getElementById('root');
if (!root) throw new Error('#root missing');

createRoot(root).render(
  <StrictMode>
    {source ? <App source={source} kiosk={kiosk} /> : <M3App />}
  </StrictMode>,
);

// Last-resort recovery for an unattended booth: if the whole tree fails, reload.
if (kiosk) {
  window.addEventListener('error', () => {
    if (!root.hasChildNodes()) window.setTimeout(() => window.location.reload(), 10_000);
  });
}
