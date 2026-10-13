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

const params = new URLSearchParams(window.location.search);
const source = createDataSource({ search: window.location.search, env: import.meta.env });
const kiosk = params.get('kiosk') === '1';

const root = document.getElementById('root');
if (!root) throw new Error('#root missing');

createRoot(root).render(
  <StrictMode>
    <App source={source} kiosk={kiosk} />
  </StrictMode>,
);

// Last-resort recovery for an unattended booth: if the whole tree fails, reload.
if (kiosk) {
  window.addEventListener('error', () => {
    if (!root.hasChildNodes()) window.setTimeout(() => window.location.reload(), 10_000);
  });
}
