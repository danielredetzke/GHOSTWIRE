'use strict';
// Web UI for the WireGuard manager. Plain JavaScript, no build step. All
// data comes from /api/v1. Text from the server is always inserted as text
// nodes, never as HTML.
(() => {
  const APP = 'GHOSTWIRE';
  const app = document.getElementById('app');
  let me = null;
  let main = null;
  let cleanups = [];

  // ---------- DOM helpers ----------

  const PROPS = new Set(['value', 'checked', 'disabled', 'selected', 'hidden', 'readOnly', 'htmlFor', 'type', 'name', 'id', 'href', 'src', 'alt', 'download', 'placeholder', 'autocomplete', 'inputMode', 'min', 'max', 'rows', 'accept', 'required', 'title']);

  function h(tag, props, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'style') Object.assign(el.style, v);
      else if (k.startsWith('on')) el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (PROPS.has(k)) el[k] = v;
      else el.setAttribute(k, v === true ? '' : v);
    }
    add(el, kids);
    return el;
  }

  function add(el, kids) {
    for (const k of kids.flat(Infinity)) {
      if (k == null || k === false) continue;
      el.append(k instanceof Node ? k : String(k));
    }
    return el;
  }

  // fill replaces the children of el; null and false parts are skipped
  // (replaceChildren would print them as text).
  function fill(el, ...kids) {
    el.replaceChildren();
    return add(el, kids);
  }

  const ICONS = {
    dashboard: '<rect x="3" y="3" width="7" height="7" rx="1.5"/><rect x="14" y="3" width="7" height="7" rx="1.5"/><rect x="3" y="14" width="7" height="7" rx="1.5"/><rect x="14" y="14" width="7" height="7" rx="1.5"/>',
    peers: '<circle cx="9" cy="8" r="3.5"/><path d="M2.5 20c.8-3.5 3.4-5.5 6.5-5.5s5.7 2 6.5 5.5"/><path d="M16 4.8a3.5 3.5 0 0 1 0 6.4M18.5 14.8c1.5.8 2.6 2.6 3 5.2"/>',
    server: '<rect x="3" y="4" width="18" height="7" rx="1.5"/><rect x="3" y="13" width="18" height="7" rx="1.5"/><path d="M7 7.5h.01M7 16.5h.01"/>',
    settings: '<path d="M4 6h10M18 6h2M4 12h4M12 12h8M4 18h12"/><circle cx="16" cy="6" r="2"/><circle cx="10" cy="12" r="2"/><circle cx="18" cy="18" r="2"/>',
    plus: '<path d="M12 5v14M5 12h14"/>',
  };

  // The Kamon logo: a crest ring around the ghost. Same drawing as favicon.svg.
  const LOGO = '<rect width="64" height="64" rx="14" fill="#1b1b1d"/><rect x="0.5" y="0.5" width="63" height="63" rx="13.5" fill="none" stroke="#fff" stroke-opacity="0.2"/>' +
    '<circle cx="32" cy="32" r="22" fill="none" stroke="#fff" stroke-width="3.5"/><g transform="translate(32 33) scale(0.66) translate(-32 -33)">' +
    '<path d="M18 50V30a14 14 0 0 1 28 0v20l-4.7-4-4.6 4-4.7-4-4.7 4-4.6-4z" fill="#fff"/><circle cx="27" cy="30" r="3.2" fill="#c8372d"/><circle cx="37" cy="30" r="3.2" fill="#c8372d"/></g>';

  function logo(size) {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    for (const [k, v] of Object.entries({ width: size, height: size, viewBox: '0 0 64 64', 'aria-hidden': 'true' })) s.setAttribute(k, v);
    s.innerHTML = LOGO;
    return s;
  }

  // brand is the logo lockup: mark, wordmark and the katakana reading.
  const brand = (size) => h('div', { class: 'brand' }, logo(size),
    h('div', null, h('div', { class: 'wm' }, APP), h('div', { class: 'kana', lang: 'ja' }, 'ゴーストワイヤー')));

  // Icons are constant markup from ICONS, never data.
  function icon(name, size = 18, width = 1.7) {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    for (const [k, v] of Object.entries({ width: size, height: size, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': width, 'stroke-linecap': 'round', 'aria-hidden': 'true' })) s.setAttribute(k, v);
    s.innerHTML = ICONS[name];
    return s;
  }

  // ---------- formatting ----------

  function fmtBytes(n) {
    if (n == null) return '–';
    const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
    let i = 0, v = n;
    while (v >= 1000 && i < u.length - 1) { v /= 1000; i++; }
    const s = i === 0 ? String(v) : v < 10 ? v.toFixed(2) : v < 100 ? v.toFixed(1) : String(Math.round(v));
    return s + ' ' + u[i];
  }

  function ago(iso) {
    if (!iso) return 'never';
    const s = Math.max(0, (Date.now() - Date.parse(iso)) / 1000);
    if (s < 60) return Math.round(s) + ' s ago';
    if (s < 3600) return Math.round(s / 60) + ' min ago';
    if (s < 86400) return Math.round(s / 3600) + ' h ago';
    return Math.round(s / 86400) + ' d ago';
  }

  const fmtDate = (iso) => iso ? new Date(iso).toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric' }) : '–';

  function fmtWhen(iso) {
    const d = new Date(iso), now = new Date();
    if (d.toDateString() === now.toDateString()) return d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
    const y = new Date(now); y.setDate(now.getDate() - 1);
    if (d.toDateString() === y.toDateString()) return 'Yest.';
    return d.toLocaleDateString(undefined, { day: 'numeric', month: 'short' });
  }

  const list = (s) => s.split(',').map((x) => x.trim()).filter(Boolean);
  const sameList = (a, b) => JSON.stringify(a || null) === JSON.stringify(b || null);

  function peerState(p) {
    if (!p.enabled) return { key: 'disabled', label: 'Disabled', dot: 'dot bad' };
    if (p.stats.online) return { key: 'online', label: 'Online · ' + ago(p.stats.lastHandshake), dot: 'dot ok' };
    if (p.stats.lastHandshake) return { key: 'offline', label: 'Offline · ' + ago(p.stats.lastHandshake), dot: 'dot' };
    return { key: 'never', label: 'Never connected', dot: 'dot off' };
  }

  // fmtLogLine turns a JSON log record into "2026-10-03 13:46:25  INFO   login  actor=admin ...".
  function fmtLogLine(l) {
    const t = new Date(l.time);
    const pad = (n) => String(n).padStart(2, '0');
    const ts = t.getFullYear() + '-' + pad(t.getMonth() + 1) + '-' + pad(t.getDate()) + ' ' + pad(t.getHours()) + ':' + pad(t.getMinutes()) + ':' + pad(t.getSeconds());
    const rest = Object.entries(l).filter(([k]) => !['time', 'level', 'msg', 'audit'].includes(k))
      .map(([k, v]) => k + '=' + (typeof v === 'string' ? v : JSON.stringify(v))).join(' ');
    return ts + '  ' + String(l.level).padEnd(5) + '  ' + l.msg + (rest ? '  ' + rest : '');
  }

  // "Germany · Deutsche Telekom AG", "Local network" or "".
  function fmtLocation(g) {
    if (!g) return '';
    return [g.countryName || g.country, g.network].filter(Boolean).join(' · ');
  }

  function fmtDuration(sec) {
    if (sec < 60) return 'under 1 min';
    const m = Math.round(sec / 60);
    if (m < 60) return m + ' min';
    const h = Math.floor(m / 60);
    if (h < 48) return h + ' h ' + (m % 60) + ' min';
    return Math.round(h / 24) + ' days';
  }

  const fmtStamp = (iso) => new Date(iso).toLocaleString(undefined, { day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

  const badge = (st) => h('span', { class: 'badge' }, h('span', { class: st.dot }), st.label);

  // ---------- API ----------

  async function api(method, path, body) {
    const opt = { method, credentials: 'same-origin', headers: {} };
    if (body !== undefined) {
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    const r = await fetch('/api/v1' + path, opt);
    let data = {};
    try { data = await r.json(); } catch { /* empty body */ }
    if (r.status === 401 && path !== '/auth/login' && path !== '/auth/me') {
      me = null;
      showLogin();
      throw new Error('Signed out');
    }
    if (!r.ok) throw new Error(data.error || r.statusText);
    return data;
  }

  // ---------- toasts & dialogs ----------

  function toast(msg, err) {
    const t = h('div', { class: err ? 'toast err' : 'toast' }, msg);
    document.getElementById('toasts').append(t);
    setTimeout(() => t.remove(), err ? 8000 : 4000);
  }

  // applied reports a kernel error that happened after a successful save.
  function applied(res, okMsg) {
    if (res && res.applyError) toast('Saved, but applying to WireGuard failed: ' + res.applyError, true);
    else if (okMsg) toast(okMsg);
  }

  function dialog(build) {
    const d = h('dialog');
    const close = () => d.close();
    d.addEventListener('close', () => d.remove());
    d.append(build(close));
    document.body.append(d);
    d.showModal();
    return d;
  }

  function confirmDialog({ title, text, ok = 'Confirm', danger = false }) {
    return new Promise((resolve) => {
      let result = false;
      const d = dialog((close) => h('div', { class: 'dlg' },
        h('h2', null, title),
        h('p', null, text),
        h('div', { class: 'foot' },
          h('button', { type: 'button', class: 'btn', onClick: close }, 'Cancel'),
          h('button', { type: 'button', class: danger ? 'btn danger' : 'btn primary', onClick: () => { result = true; close(); } }, ok))));
      d.addEventListener('close', () => resolve(result));
    });
  }

  function download(name, text, type = 'text/plain') {
    const url = URL.createObjectURL(new Blob([text], { type }));
    const a = h('a', { href: url, download: name });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  async function copy(text) {
    try { await navigator.clipboard.writeText(text); toast('Copied'); } catch { toast('Copy failed: your browser blocked clipboard access', true); }
  }

  // configDialog shows a freshly issued client config. It is the only time
  // the private key exists anywhere.
  function configDialog(res, onClose) {
    const name = res.peer.name;
    const d = dialog((close) => h('div', { class: 'dlg' },
      h('h2', null, 'Config for ' + name),
      res.includesPrivateKey
        ? h('div', { class: 'notice' }, 'This is the only time the private key is shown. Scan or download it now: it is not stored on the server.')
        : h('div', { class: 'notice' }, 'The device keeps its own private key. Put it into the PrivateKey line.'),
      h('div', { class: 'qrrow' },
        res.qr ? h('img', { class: 'qr', src: res.qr, alt: 'QR code of the client config for ' + name }) : null,
        h('div', { class: 'col' },
          h('button', { type: 'button', class: 'btn', onClick: () => download(name + '.conf', res.config) }, 'Download ' + name + '.conf'),
          h('button', { type: 'button', class: 'btn', onClick: () => copy(res.config) }, 'Copy config'))),
      h('pre', { class: 'code' }, res.config),
      h('div', { class: 'foot' }, h('button', { type: 'button', class: 'btn primary', onClick: close }, 'Done'))));
    d.addEventListener('close', () => { applied(res); if (onClose) onClose(); });
  }

  // ---------- chart ----------

  function niceTop(max) {
    if (max <= 0) return 1e6;
    const e = Math.pow(10, Math.floor(Math.log10(max)));
    for (const m of [1, 2, 5, 10]) if (m * e >= max) return m * e;
    return 10 * e;
  }

  function pointLabel(t, range) {
    const d = new Date(t * 1000);
    if (range === '24h') {
      const hrs = Math.round((Date.now() - d.getTime()) / 3600000);
      return hrs <= 0 ? 'This hour' : hrs + ' h ago';
    }
    return d.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' });
  }

  // chart draws bars for each point: two per point (download, upload) in
  // "pair" mode, one stacked total in "total" mode. Hover or focus shows the
  // values in the readout line above.
  function chart(points, range, mode, small) {
    const H = small ? 178 : 198;
    const vals = points.map((p) => mode === 'pair' ? Math.max(p.down, p.up) : p.down + p.up);
    const top = niceTop(Math.max(0, ...vals));
    const px = (v) => v > 0 ? Math.max(2, Math.round(v / top * H)) + 'px' : '0px';
    let peakIdx = vals.indexOf(Math.max(...vals));
    const describe = (i) => {
      const p = points[i];
      return mode === 'pair'
        ? [pointLabel(p.t, range), ' · Download ', h('strong', null, fmtBytes(p.down)), ' · Upload ', h('strong', null, fmtBytes(p.up))]
        : [h('strong', null, fmtBytes(p.down + p.up)), ' · ', pointLabel(p.t, range)];
    };
    const idle = () => mode === 'pair' || vals[peakIdx] === 0
      ? ['Hover or focus a bar to see its values.']
      : [h('strong', null, fmtBytes(vals[peakIdx])), ' · peak, ', pointLabel(points[peakIdx].t, range)];
    const readout = h('div', { class: 'readout' }, idle());
    const groups = points.map((p, i) => {
      const spans = mode === 'pair'
        ? [h('span', { class: 'down', style: { height: px(p.down) } }), h('span', { class: 'up', style: { height: px(p.up) } })]
        : [h('span', { class: 'total', style: { height: px(p.down + p.up) } })];
      const label = pointLabel(p.t, range) + ': download ' + fmtBytes(p.down) + ', upload ' + fmtBytes(p.up);
      const show = () => {
        groups.forEach((g) => g.classList.remove('on'));
        g.classList.add('on');
        readout.replaceChildren(...describe(i));
      };
      const g = h('button', { type: 'button', class: 'grp', 'aria-label': label, onMouseenter: show, onFocus: show }, spans);
      return g;
    });
    const reset = () => { groups.forEach((g) => g.classList.remove('on')); readout.replaceChildren(...idle()); };
    const bars = h('div', { class: 'bars', onMouseleave: reset, onFocusout: reset }, groups);
    return h('div', null,
      readout,
      h('div', { class: small ? 'chart small' : 'chart' },
        h('div', { class: 'gl top' }), h('div', { class: 'gl mid' }), h('div', { class: 'gl base' }),
        h('div', { class: 'yl top' }, fmtBytes(top)), h('div', { class: 'yl mid' }, fmtBytes(top / 2)),
        bars),
      h('div', { class: 'xaxis' },
        h('span', null, pointLabel(points[0].t, range)),
        h('span', null, range === '24h' ? 'now' : pointLabel(points[points.length - 1].t, range))));
  }

  // ---------- shell, router ----------

  const NAV = [['#/', 'dashboard', 'Dashboard'], ['#/peers', 'peers', 'Peers'], ['#/server', 'server', 'Server'], ['#/settings', 'settings', 'Settings']];
  let navLinks = {};
  let srvBox, peerCount;

  function buildShell() {
    srvBox = h('div', { class: 'srv' }, h('span', { class: 'dot' }), h('span', null, 'Loading…'));
    peerCount = h('span', { class: 'count' });
    navLinks = {};
    const nav = h('nav', { class: 'side', 'aria-label': 'Main' },
      brand(34),
      srvBox,
      NAV.map(([href, ic, label]) => (navLinks[href] = h('a', { class: 'nav', href }, icon(ic), label, ic === 'peers' ? peerCount : null))),
      h('div', { class: 'foot' },
        'Signed in as ', me.name, ' · ',
        h('button', { type: 'button', onClick: logout }, 'Sign out'),
        h('br'), 'v' + me.version));
    main = h('main', { class: 'main', id: 'main' });
    app.replaceChildren(h('div', { class: 'shell' }, nav, main));
    refreshSide();
  }

  async function refreshSide() {
    try {
      const s = await api('GET', '/status');
      const up = s.checks.find((c) => c.name === 'WireGuard interface');
      const ok = up && up.ok;
      srvBox.replaceChildren(
        h('span', { class: s.healthy ? 'dot ok' : 'dot bad' }),
        h('span', null, h('strong', null, s.interface + (ok ? ' up' : ' down')), ' · ' + s.listenPort + '/udp', s.healthy ? '' : ' · check health'));
      peerCount.textContent = s.peers.total;
    } catch { /* shown on next refresh */ }
  }

  async function logout() {
    try { await api('POST', '/auth/logout'); } catch { /* ignore */ }
    me = null;
    showLogin();
  }

  function every(ms, fn) {
    const id = setInterval(() => { if (!document.hidden) fn(); }, ms);
    cleanups.push(() => clearInterval(id));
  }

  const ROUTES = [
    [/^#\/?$/, '#/', viewDashboard],
    [/^#\/peers$/, '#/peers', viewPeers],
    [/^#\/peers\/new$/, '#/peers', viewPeerNew],
    [/^#\/peers\/([\w-]+)$/, '#/peers', viewPeer],
    [/^#\/server$/, '#/server', viewServer],
    [/^#\/settings$/, '#/settings', viewSettings],
  ];

  async function render() {
    cleanups.forEach((f) => f());
    cleanups = [];
    if (!me) {
      try { me = await api('GET', '/auth/me'); } catch { showLogin(); return; }
    }
    if (!main || !main.isConnected) buildShell();
    every(30000, refreshSide);
    const hash = location.hash || '#/';
    const route = ROUTES.find(([re]) => re.test(hash)) || ROUTES[0];
    for (const [href, a] of Object.entries(navLinks)) {
      a.classList.toggle('on', href === route[1]);
      if (href === route[1]) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
    const args = hash.match(route[0]).slice(1);
    const wrap = h('div', { class: 'wrap' }, h('p', { class: 'muted' }, 'Loading…'));
    main.replaceChildren(wrap);
    window.scrollTo(0, 0);
    try {
      await route[2](wrap, ...args);
    } catch (e) {
      if (e.message !== 'Signed out') fill(wrap, h('div', { class: 'notice err' }, e.message));
    }
  }

  window.addEventListener('hashchange', render);

  // ---------- login ----------

  function showLogin() {
    cleanups.forEach((f) => f());
    cleanups = [];
    main = null;
    const err = h('p', { class: 'err-text', role: 'alert' });
    const user = h('input', { id: 'u', autocomplete: 'username', value: 'admin', required: true });
    const pw = h('input', { id: 'p', type: 'password', autocomplete: 'current-password', required: true });
    const btn = h('button', { type: 'submit', class: 'btn primary' }, 'Sign in');
    const form = h('form', { class: 'loginform', onSubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      btn.disabled = true;
      try {
        await api('POST', '/auth/login', { username: user.value, password: pw.value });
        me = await api('GET', '/auth/me');
        render();
      } catch (x) {
        err.textContent = x.message;
        btn.disabled = false;
        pw.select();
      }
    } },
    h('div', { class: 'field' }, h('label', { htmlFor: 'u' }, 'Username'), user),
    h('div', { class: 'field' }, h('label', { htmlFor: 'p' }, 'Password'), pw),
    err, btn);
    app.replaceChildren(h('div', { class: 'loginpage' }, h('div', { class: 'loginbox' },
      brand(72),
      form),
      h('p', { class: 'loginfoot' }, 'WireGuard server manager')));
    pw.focus();
  }

  // ---------- dashboard ----------

  async function viewDashboard(wrap) {
    const draw = async () => {
      const [st, pl, stats, logs] = await Promise.all([
        api('GET', '/status'),
        api('GET', '/peers'),
        api('GET', '/stats?range=24h'),
        me.isAdmin ? api('GET', '/logs?audit=1&limit=6').catch(() => null) : null,
      ]);
      const peers = pl.peers;
      const failing = st.checks.filter((c) => !c.ok);
      const ifCheck = st.checks.find((c) => c.name === 'WireGuard interface');
      const top = [...peers].sort((a, b) => (b.stats.down24h + b.stats.up24h) - (a.stats.down24h + a.stats.up24h)).slice(0, 6);

      fill(wrap,
        h('div', { class: 'head' },
          h('div', null, h('h1', null, 'Dashboard'),
            h('p', { class: 'sub' }, 'Endpoint ', h('span', { class: 'mono' }, st.endpoint), ' · network ', h('span', { class: 'mono' }, st.ipv4))),
          h('div', { class: 'actions' },
            h('a', { class: 'btn', href: '#/server' }, 'Server config'),
            h('a', { class: 'btn primary', href: '#/peers/new' }, icon('plus', 16, 2), 'Add peer'))),

        failing.length ? h('div', { class: 'notice err', role: 'alert' },
          h('div', null, h('strong', null, 'Needs attention: '), failing.map((c) => c.name + ' (' + c.detail + ')').join(' · ')),
          h('a', { class: 'btn small', href: '#/server' }, 'Health')) : null,

        h('div', { class: 'tiles' },
          h('div', { class: 'card tile' }, h('div', { class: 'k' }, 'Peers online'),
            h('div', { class: 'v' }, String(st.peers.online), h('small', null, '/ ' + st.peers.total)),
            h('div', { class: 's' }, st.peers.disabled + ' disabled · ' + st.peers.never + ' never connected')),
          h('div', { class: 'card tile' }, h('div', { class: 'k' }, 'Traffic, last 24 h'),
            h('div', { class: 'v' }, fmtBytes(st.traffic24h.down + st.traffic24h.up)),
            h('div', { class: 's' }, 'Download ' + fmtBytes(st.traffic24h.down) + ' · Upload ' + fmtBytes(st.traffic24h.up))),
          h('div', { class: 'card tile' }, h('div', { class: 'k' }, 'Traffic, last 30 days'),
            h('div', { class: 'v' }, fmtBytes(st.traffic30d.down + st.traffic30d.up)),
            h('div', { class: 's' }, st.topPeer30d ? 'Top peer: ' + st.topPeer30d : 'No traffic yet')),
          h('div', { class: 'card tile' }, h('div', { class: 'k' }, 'Interface'),
            h('div', { class: 'v' }, h('span', { class: ifCheck && ifCheck.ok ? 'dot ok big' : 'dot bad big' }), ifCheck && ifCheck.ok ? 'Up' : 'Down'),
            h('div', { class: 's' }, 'Service up ' + ago(st.started).replace(' ago', '') + ' · ' + (st.healthy ? 'all checks pass' : failing.length + ' check(s) failing')))),

        h('section', { class: 'card', 'aria-labelledby': 'tput' },
          h('div', { class: 'cardhead' }, h('h2', { id: 'tput' }, 'Traffic, all peers · last 24 hours')),
          chart(stats.points, '24h', 'total', true)),

        h('div', { class: 'cols' },
          h('section', { class: 'card flush' },
            h('div', { class: 'cardhead' }, h('h2', null, 'Peers'), h('a', { href: '#/peers' }, 'All peers')),
            top.length ? h('div', { class: 'tbl' }, h('table', { class: 'narrow' },
              h('thead', null, h('tr', null, h('th', null, 'Name'), h('th', null, 'Status'), h('th', { class: 'num' }, 'Download, 24 h'), h('th', { class: 'num' }, 'Upload, 24 h'))),
              h('tbody', null, top.map((p) => h('tr', null,
                h('td', null, h('a', { href: '#/peers/' + p.id }, p.name)),
                h('td', null, badge(peerState(p))),
                h('td', { class: 'num' }, fmtBytes(p.stats.down24h)),
                h('td', { class: 'num' }, fmtBytes(p.stats.up24h)))))))
              : h('p', { class: 'empty' }, 'No peers yet. ', h('a', { href: '#/peers/new' }, 'Add the first one'))),
          logs ? h('section', { class: 'card' },
            h('div', { class: 'cardhead' }, h('h2', null, 'Recent activity'), h('a', { href: '#/settings' }, 'Log')),
            logs.lines.length
              ? h('div', null, logs.lines.map((l) => h('div', { class: 'ev' }, h('time', { datetime: l.time }, fmtWhen(l.time)), h('span', null, describeAudit(l)))))
              : h('p', { class: 'empty' }, 'No changes yet.')) : null));
    };
    await draw();
    every(30000, () => draw().catch(() => {}));
  }

  function describeAudit(l) {
    const parts = [l.msg.charAt(0).toUpperCase() + l.msg.slice(1)];
    if (l.peer) parts.push(': ' + l.peer);
    if (l.token) parts.push(': ' + l.token);
    if (l.fields && l.fields.length) parts.push(' (' + l.fields.join(', ') + ')');
    parts.push(' · ' + (l.actor || ''));
    return parts.join('');
  }

  // ---------- peers ----------

  async function viewPeers(wrap) {
    let q = '', filter = 'all', data = await api('GET', '/peers');
    const tbody = h('tbody');
    const empty = h('p', { class: 'empty', hidden: true }, 'No peers match this filter.');
    const sub = h('p', { class: 'sub' });
    const pills = h('div', { class: 'pills', role: 'group', 'aria-label': 'Status filter' });

    const drawPills = () => pills.replaceChildren(...[['all', 'All'], ['online', 'Online'], ['offline', 'Offline'], ['disabled', 'Disabled']].map(([k, label]) =>
      h('button', { type: 'button', class: filter === k ? 'pill on' : 'pill', 'aria-pressed': String(filter === k), onClick: () => { filter = k; drawPills(); drawRows(); } }, label)));

    const toggle = async (p, on) => {
      try {
        const res = await api('POST', '/peers/' + p.id + (on ? '/enable' : '/disable'));
        applied(res, (on ? 'Enabled ' : 'Disabled ') + p.name);
        data = await api('GET', '/peers');
        drawRows();
      } catch (e) { toast(e.message, true); drawRows(); }
    };

    const drawRows = () => {
      sub.replaceChildren(data.peers.length + ' of ' + data.capacity + ' addresses in ', h('span', { class: 'mono' }, data.network), ' used');
      const rows = data.peers.filter((p) => {
        const st = peerState(p).key;
        const hit = !q || (p.name + ' ' + p.ipv4 + ' ' + p.note).toLowerCase().includes(q);
        const keep = filter === 'all' || filter === st || (filter === 'offline' && (st === 'offline' || st === 'never'));
        return hit && keep;
      });
      tbody.replaceChildren(...rows.map((p) => h('tr', null,
        h('td', null, h('a', { href: '#/peers/' + p.id }, h('strong', null, p.name)), p.note ? h('div', { class: 'note' }, p.note) : null),
        h('td', { class: 'mono' }, p.ipv4),
        h('td', null, badge(peerState(p))),
        h('td', { class: 'mono muted' }, p.stats.endpoint || '–',
          p.stats.location && p.stats.location.country ? h('span', { class: 'cc', title: fmtLocation(p.stats.location) }, p.stats.location.country) : null),
        h('td', { class: 'num' }, fmtBytes(p.stats.down30d)),
        h('td', { class: 'num' }, fmtBytes(p.stats.up30d)),
        h('td', null, h('input', { type: 'checkbox', class: 'sw', checked: p.enabled, 'aria-label': (p.enabled ? 'Disable ' : 'Enable ') + p.name, onChange: (e) => toggle(p, e.target.checked) })),
        h('td', { class: 'num' }, h('a', { class: 'btn small', href: '#/peers/' + p.id }, 'Manage')))));
      empty.hidden = rows.length > 0;
    };

    drawPills();
    drawRows();
    fill(wrap,
      h('div', { class: 'head' },
        h('div', null, h('h1', null, 'Peers'), sub),
        h('div', { class: 'actions' }, h('a', { class: 'btn primary', href: '#/peers/new' }, icon('plus', 16, 2), 'Add peer'))),
      h('div', { class: 'row', style: { flexWrap: 'wrap', alignItems: 'center', gap: '10px' } },
        h('label', { htmlFor: 'q', class: 'sr' }, 'Search peers'),
        h('input', { id: 'q', type: 'search', placeholder: 'Search name, address or note', style: { flex: '1 1 260px', maxWidth: '360px' }, onInput: (e) => { q = e.target.value.toLowerCase(); drawRows(); } }),
        pills),
      h('section', { class: 'card flush' }, h('div', { class: 'tbl' }, h('table', null,
        h('thead', null, h('tr', null, ['Name', 'Address', 'Status', 'Endpoint'].map((t) => h('th', null, t)),
          h('th', { class: 'num' }, 'Download, 30 d'), h('th', { class: 'num' }, 'Upload, 30 d'), h('th', null, 'Enabled'), h('th', null, h('span', { class: 'sr' }, 'Actions')))),
        tbody), empty)),
      h('p', { class: 'muted', style: { margin: '0', fontSize: '13px' } }, 'Online means a handshake in the last 3 minutes. Download and Upload are measured from the peer\'s side. Changes apply live without disconnecting other peers.'));
    every(15000, async () => { try { data = await api('GET', '/peers'); drawRows(); } catch { /* keep last */ } });
  }

  // ---------- shared peer form pieces ----------

  // choice builds a select with "server default" plus options, and a text
  // input that appears for "custom".
  function choice({ id, label, hint, options, value, customValue, placeholder, mono = true, onChange }) {
    const input = h('input', { id: id + '-c', class: mono ? 'mono' : null, value: customValue || '', placeholder, hidden: value !== 'custom', 'aria-label': label + ' (custom)' });
    const sel = h('select', { id }, options.map(([v, t]) => h('option', { value: v, selected: v === value }, t)));
    const sync = () => { input.hidden = sel.value !== 'custom'; if (onChange) onChange(); };
    sel.addEventListener('change', sync);
    input.addEventListener('input', () => onChange && onChange());
    const el = h('div', { class: 'field' }, h('label', { htmlFor: id }, label), sel, input, hint ? h('span', { class: 'hint' }, hint) : null);
    return { el, get: () => sel.value, custom: () => input.value };
  }

  function serverNets(srv) {
    const nets = [srv.ipv4];
    if (srv.ipv6Enabled) nets.push(srv.ipv6);
    return nets;
  }

  // Reads the DNS / AllowedIPs / keepalive choices into API values; null
  // means "use the server default".
  function overrides(dns, allowed, ka, srv) {
    const out = {};
    out.dns = dns.get() === 'default' ? null : list(dns.custom());
    const a = allowed.get();
    out.allowedIPs = a === 'default' ? null : a === 'vpn' ? serverNets(srv) : list(allowed.custom());
    const k = ka.get();
    out.keepalive = k === 'default' ? null : k === 'off' ? 0 : parseInt(ka.custom(), 10);
    if (k === 'custom' && !(out.keepalive >= 0)) throw new Error('Keepalive must be a number of seconds');
    return out;
  }

  function overrideChoices(srv, p) {
    const d = srv.clientDefaults;
    const allowedMode = !p || p.allowedIPs == null ? 'default' : sameList(p.allowedIPs, serverNets(srv)) ? 'vpn' : 'custom';
    return {
      dns: { options: [['default', 'Server default · ' + (d.dns.join(', ') || 'none')], ['custom', 'Custom…']], value: !p || p.dns == null ? 'default' : 'custom', customValue: p && p.dns ? p.dns.join(', ') : '' },
      allowed: { options: [['default', 'Server default · ' + d.allowedIPs.join(', ')], ['vpn', 'Only the VPN network'], ['custom', 'Custom…']], value: allowedMode, customValue: p && p.allowedIPs ? p.allowedIPs.join(', ') : '' },
      ka: { options: [['default', 'Server default · ' + (d.keepalive ? d.keepalive + ' s' : 'off')], ['off', 'Off'], ['custom', 'Custom…']], value: !p || p.keepalive == null ? 'default' : p.keepalive === 0 ? 'off' : 'custom', customValue: p && p.keepalive ? String(p.keepalive) : '' },
    };
  }

  // ---------- add peer ----------

  async function viewPeerNew(wrap) {
    const srv = await api('GET', '/server');
    const err = h('p', { class: 'err-text', role: 'alert' });
    const name = h('input', { id: 'n', autocomplete: 'off', required: true });
    const note = h('input', { id: 'no' });
    const ip = h('input', { id: 'ip', class: 'mono', placeholder: 'Next free address' });
    const pub = h('textarea', { id: 'pk', class: 'mono', rows: 2, placeholder: 'Base64 public key from the device', hidden: true, 'aria-label': 'Client public key' });
    const psk = h('input', { type: 'checkbox', checked: true });
    const preview = h('pre', { class: 'code' });
    const ch = overrideChoices(srv, null);
    const update = () => drawPreview();
    const dns = choice({ id: 'dns', label: 'DNS', ...ch.dns, placeholder: '9.9.9.9, 149.112.112.112', onChange: update });
    const allowed = choice({ id: 'ai', label: 'Route through the VPN (AllowedIPs)', ...ch.allowed, placeholder: '10.0.0.0/24, 192.168.1.0/24', hint: 'Used in the client config', onChange: update });
    const ka = choice({ id: 'ka', label: 'Persistent keepalive', ...ch.ka, placeholder: 'Seconds', hint: 'Keeps the tunnel open behind NAT', onChange: update });
    let keyMode = 'generate';

    function drawPreview() {
      let o;
      try { o = overrides(dns, allowed, ka, srv); } catch { o = {}; }
      const d = srv.clientDefaults;
      const lines = ['[Interface]',
        'PrivateKey = ' + (keyMode === 'generate' ? '‹generated on create›' : '‹stays on the device›'),
        'Address = ' + (ip.value || '‹next free›') + '/' + srv.ipv4.split('/')[1] + (srv.ipv6Enabled ? ',‹mapped IPv6›' : '')];
      const dnsList = o.dns === undefined || o.dns === null ? d.dns : o.dns;
      if (dnsList.length) lines.push('DNS = ' + dnsList.join(', '));
      lines.push('', '[Peer]', 'PublicKey = ' + srv.publicKey);
      if (psk.checked) lines.push('PresharedKey = ‹generated on create›');
      lines.push('Endpoint = ' + (srv.endpoint || '‹set the endpoint in Server›') + ':' + (srv.endpointPort || srv.listenPort));
      lines.push('AllowedIPs = ' + ((o.allowedIPs == null ? d.allowedIPs : o.allowedIPs).join(', ')));
      const k = o.keepalive == null ? d.keepalive : o.keepalive;
      if (k > 0) lines.push('PersistentKeepalive = ' + k);
      preview.textContent = lines.join('\n');
    }

    const keyOpt = (v, title, hint) => h('label', { class: 'opt' },
      h('input', { type: 'radio', name: 'keys', value: v, checked: v === keyMode, onChange: () => { keyMode = v; pub.hidden = v !== 'paste'; drawPreview(); } }),
      h('span', null, h('strong', null, title), h('br'), h('span', { class: 'hint' }, hint)));

    const submit = h('button', { type: 'submit', class: 'btn primary' }, 'Create peer');
    const form = h('form', { class: 'card grow', onSubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      let body;
      try {
        body = { name: name.value.trim(), note: note.value.trim(), ipv4: ip.value.trim(), presharedKey: psk.checked, ...overrides(dns, allowed, ka, srv) };
      } catch (x) { err.textContent = x.message; return; }
      if (keyMode === 'paste') body.publicKey = pub.value.trim();
      submit.disabled = true;
      try {
        const res = await api('POST', '/peers', body);
        configDialog(res, () => { location.hash = '#/peers/' + res.peer.id; });
      } catch (x) {
        err.textContent = x.message;
        submit.disabled = false;
      }
    } },
    h('div', { class: 'grid' },
      h('div', { class: 'field' }, h('label', { htmlFor: 'n' }, 'Name'), name, h('span', { class: 'hint' }, 'Letters, numbers, . _ @ - · max 32 · unique')),
      h('div', { class: 'field' }, h('label', { htmlFor: 'no' }, 'Note ', h('span', { class: 'hint' }, '(optional)')), note),
      h('div', { class: 'field' }, h('label', { htmlFor: 'ip' }, 'IPv4 address'), ip, h('span', { class: 'hint' }, 'Leave empty for the next free address in ' + srv.ipv4)),
      srv.ipv6Enabled ? h('div', { class: 'field' }, h('label', null, 'IPv6 address'), h('input', { class: 'mono', readOnly: true, value: 'Derived from the IPv4 address' })) : null),
    h('div', { class: 'grid section' }, allowed.el, dns.el, ka.el),
    h('fieldset', { class: 'section' },
      h('legend', { class: 'legend' }, 'Keys'),
      keyOpt('generate', 'Generate here', 'The private key appears once in the config and QR code. It isn\'t stored.'),
      keyOpt('paste', 'Paste the client\'s public key', 'For clients that make their own keys'),
      pub,
      h('label', { class: 'check' }, psk, 'Add a preshared key')),
    err,
    h('div', { class: 'formfoot' }, h('a', { class: 'btn', href: '#/peers' }, 'Cancel'), submit));
    for (const el of [ip, psk]) el.addEventListener('input', drawPreview);
    psk.addEventListener('change', drawPreview);
    drawPreview();

    fill(wrap,
      h('a', { class: 'back', href: '#/peers' }, '← Peers'),
      h('div', null, h('h1', null, 'Add peer'), h('p', { class: 'sub' }, 'Creates a key pair, assigns the next free address and adds the peer to ' + srv.interface + ' without a restart.')),
      h('div', { class: 'split' }, form,
        h('aside', { class: 'card aside', 'aria-labelledby': 'pv' },
          h('h2', { id: 'pv' }, 'Client config preview'),
          h('p', { class: 'lead' }, 'Keys are created when you click Create peer. Then the config can be downloaded or scanned once.'),
          preview,
          h('div', { class: 'qrrow section' }, h('div', { class: 'ph' }, 'QR code', h('br'), 'after creation')))));
    name.focus();
  }

  // ---------- peer detail ----------

  async function viewPeer(wrap, id) {
    const [p, srv, sess] = await Promise.all([api('GET', '/peers/' + id), api('GET', '/server'), api('GET', '/peers/' + id + '/sessions?limit=50')]);
    const sessions = sess.sessions;
    let range = '7d';
    const st = peerState(p);
    const traffic = h('div');
    const totals = h('div', { class: 'legend-row' });
    const pills = h('div', { class: 'pills', role: 'group', 'aria-label': 'Time range' });

    async function drawTraffic() {
      pills.replaceChildren(...[['24h', '24 h'], ['7d', '7 days'], ['30d', '30 days']].map(([k, t]) =>
        h('button', { type: 'button', class: range === k ? 'pill on' : 'pill', 'aria-pressed': String(range === k), onClick: () => { range = k; drawTraffic(); } }, t)));
      const s = await api('GET', '/peers/' + id + '/stats?range=' + range);
      const down = s.points.reduce((a, x) => a + x.down, 0), up = s.points.reduce((a, x) => a + x.up, 0);
      totals.replaceChildren(
        h('span', null, h('span', { class: 'key down' }), 'Download ', h('strong', null, fmtBytes(down))),
        h('span', null, h('span', { class: 'key up' }), 'Upload ', h('strong', null, fmtBytes(up))));
      traffic.replaceChildren(chart(s.points, range, 'pair', false));
    }

    const toggle = async () => {
      try {
        const res = await api('POST', '/peers/' + id + (p.enabled ? '/disable' : '/enable'));
        applied(res, (p.enabled ? 'Disabled ' : 'Enabled ') + p.name);
        render();
      } catch (e) { toast(e.message, true); }
    };
    const del = async () => {
      if (!await confirmDialog({ title: 'Delete ' + p.name + '?', text: 'The device loses access immediately. Its traffic history is deleted too. This cannot be undone.', ok: 'Delete peer', danger: true })) return;
      try {
        const res = await api('DELETE', '/peers/' + id);
        applied(res, 'Deleted ' + p.name);
        location.hash = '#/peers';
      } catch (e) { toast(e.message, true); }
    };
    const reissue = async (publicKey) => {
      if (!publicKey && !await confirmDialog({ title: 'Issue a new config?', text: 'New keys are created. The device that uses the current config stops working until it gets the new one.', ok: 'Issue new config' })) return;
      try {
        const res = await api('POST', '/peers/' + id + '/issue-config', publicKey ? { publicKey } : undefined);
        configDialog(res, render);
      } catch (e) { toast(e.message, true); }
    };
    const pasteKey = () => {
      const inp = h('textarea', { class: 'mono', rows: 2, 'aria-label': 'Public key' });
      const e = h('p', { class: 'err-text' });
      dialog((close) => h('form', { class: 'dlg', onSubmit: async (ev) => {
        ev.preventDefault();
        close();
        await reissue(inp.value.trim());
      } },
      h('h2', null, 'Use a key from the device'),
      h('p', null, 'Paste the public key the device generated. The current config stops working.'),
      inp, e,
      h('div', { class: 'foot' }, h('button', { type: 'button', class: 'btn', onClick: close }, 'Cancel'), h('button', { type: 'submit', class: 'btn primary' }, 'Replace key'))));
    };

    // settings form
    const err = h('p', { class: 'err-text', role: 'alert' });
    const name = h('input', { id: 'n', value: p.name });
    const note = h('input', { id: 'no', value: p.note });
    const ip = h('input', { id: 'ip', class: 'mono', value: p.ipv4 });
    const ch = overrideChoices(srv, p);
    const dns = choice({ id: 'dns', label: 'DNS', ...ch.dns, placeholder: '9.9.9.9' });
    const allowed = choice({ id: 'ai', label: 'Client AllowedIPs', ...ch.allowed, hint: 'Used in the client config', placeholder: '0.0.0.0/0, ::/0' });
    const ka = choice({ id: 'ka', label: 'Persistent keepalive', ...ch.ka, placeholder: 'Seconds' });
    const save = async (e) => {
      e.preventDefault();
      err.textContent = '';
      try {
        const body = { name: name.value, note: note.value, ipv4: ip.value.trim(), ...overrides(dns, allowed, ka, srv) };
        const res = await api('PATCH', '/peers/' + id, body);
        applied(res, 'Saved');
        render();
      } catch (x) { err.textContent = x.message; }
    };

    fill(wrap,
      h('a', { class: 'back', href: '#/peers' }, '← Peers'),
      h('div', { class: 'head' },
        h('div', null,
          h('div', { class: 'titleline' }, h('h1', null, p.name), badge(st.key === 'online' ? { ...st, label: 'Online · handshake ' + ago(p.stats.lastHandshake) } : st)),
          h('p', { class: 'sub' }, (p.note ? p.note + ' · ' : '') + 'created ' + fmtDate(p.created))),
        h('div', { class: 'actions' },
          h('button', { type: 'button', class: 'btn', onClick: toggle }, p.enabled ? 'Disable' : 'Enable'),
          h('button', { type: 'button', class: 'btn danger', onClick: del }, 'Delete'))),

      h('section', { class: 'card', 'aria-labelledby': 'traffic' },
        h('div', { class: 'cardhead' }, h('div', null, h('h2', { id: 'traffic' }, 'Traffic'), totals), pills),
        traffic),

      h('div', { class: 'cols' },
        h('section', { class: 'card' },
          h('h2', null, 'Connection'),
          h('dl', { class: 'kv' },
            h('dt', null, 'Tunnel address'), h('dd', { class: 'mono' }, p.ipv4 + '/32', p.ipv6 ? [h('br'), p.ipv6 + '/128'] : null),
            h('dt', null, 'Endpoint'), h('dd', { class: 'mono' }, p.stats.endpoint || '–'),
            h('dt', null, 'Location'), h('dd', null, fmtLocation(p.stats.location) || '–'),
            h('dt', null, 'Latest handshake'), h('dd', null, ago(p.stats.lastHandshake)),
            h('dt', null, 'Public key'), h('dd', { class: 'mono' }, p.publicKey),
            h('dt', null, 'Preshared key'), h('dd', null, p.hasPresharedKey ? 'Set' : 'None'),
            h('dt', null, 'All-time traffic'), h('dd', null, 'Download ' + fmtBytes(p.stats.downTotal) + ' · Upload ' + fmtBytes(p.stats.upTotal)))),
        h('section', { class: 'card' },
          h('h2', null, 'Client configuration'),
          h('p', { class: 'lead' }, 'This server doesn\'t keep the peer\'s private key. To set up a device again, issue a new config. The old one stops working.'),
          h('div', { class: 'actions' },
            h('button', { type: 'button', class: 'btn', onClick: () => reissue() }, 'Issue new config & QR'),
            h('button', { type: 'button', class: 'btn', onClick: pasteKey }, 'Use a key from the device…')),
          h('p', { class: 'hint', style: { margin: '12px 0 0' } }, p.configIssued ? 'Last issued ' + fmtDate(p.configIssued) + '.' : 'Created with a key from the device.'))),

      h('section', { class: 'card flush', 'aria-labelledby': 'hist' },
        h('div', { class: 'cardhead' }, h('h2', { id: 'hist' }, 'Connection history'),
          h('span', { class: 'hint' }, 'Newest first · a new row starts when the device changes networks')),
        sessions.length ? h('div', { class: 'tbl' }, h('table', null,
          h('thead', null, h('tr', null, h('th', null, 'Started'), h('th', null, 'Duration'), h('th', null, 'From'), h('th', null, 'Address'),
            h('th', { class: 'num' }, 'Download'), h('th', { class: 'num' }, 'Upload'))),
          h('tbody', null, sessions.map((se) => h('tr', null,
            h('td', null, fmtStamp(se.start)),
            h('td', null, se.open ? [h('span', { class: 'badge' }, h('span', { class: 'dot ok' }), 'Online now'), ' ', fmtDuration(se.seconds)] : fmtDuration(se.seconds)),
            h('td', null, fmtLocation(se.geo) || h('span', { class: 'muted' }, 'Unknown')),
            h('td', { class: 'mono muted' }, se.ip),
            h('td', { class: 'num' }, fmtBytes(se.down)),
            h('td', { class: 'num' }, fmtBytes(se.up)))))))
          : h('p', { class: 'empty' }, 'No connections recorded yet.'),
        h('p', { class: 'hint', style: { margin: '4px 12px 12px' } }, 'Country and network: ',
          h('a', { href: 'https://db-ip.com', target: '_blank', rel: 'noopener' }, 'IP Geolocation by DB-IP'),
          '. Kept as long as the daily traffic history.')),

      h('form', { class: 'card', onSubmit: save },
        h('h2', null, 'Settings'),
        h('p', { class: 'lead' }, 'Name and address changes apply immediately. DNS, AllowedIPs and keepalive are part of the client config: they take effect after the config is issued again.'),
        h('div', { class: 'grid' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'n' }, 'Name'), name, h('span', { class: 'hint' }, 'Letters, numbers, . _ @ - · max 32')),
          h('div', { class: 'field' }, h('label', { htmlFor: 'no' }, 'Note'), note),
          h('div', { class: 'field' }, h('label', { htmlFor: 'ip' }, 'IPv4 address'), ip, h('span', { class: 'hint' }, 'Changing it requires a new client config')),
          ka.el, allowed.el, dns.el),
        err,
        h('div', { class: 'formfoot' },
          h('button', { type: 'button', class: 'btn', onClick: render }, 'Cancel'),
          h('button', { type: 'submit', class: 'btn primary' }, 'Save changes'))));
    await drawTraffic();
  }

  // ---------- server ----------

  const DNS_PRESETS = [['Quad9', '9.9.9.9, 149.112.112.112']];

  async function viewServer(wrap) {
    const [srv, st] = await Promise.all([api('GET', '/server'), api('GET', '/status')]);
    const orig = JSON.parse(JSON.stringify(srv));
    const draft = JSON.parse(JSON.stringify(srv));
    const LABELS = {
      listenPort: 'Listen port', mtu: 'MTU', ipv4: 'IPv4 network', ipv6: 'IPv6 network', ipv6Enabled: 'IPv6',
      endpoint: 'Endpoint host', endpointPort: 'Endpoint port', uplinkV4: 'IPv4 uplink', uplinkV6: 'IPv6 uplink',
      nat: 'NAT', peerToPeer: 'Peer-to-peer', lanAccess: 'LAN access', openPort: 'Open port', clientDefaults: 'Client defaults',
    };
    const DISRUPTIVE = ['listenPort', 'ipv4', 'ipv6', 'ipv6Enabled'];
    const bar = h('div', { class: 'applybar', role: 'region', 'aria-label': 'Pending changes', hidden: true });
    const result = h('div');

    const changed = () => Object.keys(LABELS).filter((k) => JSON.stringify(draft[k]) !== JSON.stringify(orig[k]));
    const drawBar = () => {
      const c = changed();
      bar.hidden = c.length === 0;
      if (!c.length) return;
      const disrupt = c.some((k) => DISRUPTIVE.includes(k));
      bar.replaceChildren(
        h('span', null, c.length + ' unsaved change' + (c.length > 1 ? 's' : '') + ': ' + c.map((k) => LABELS[k]).join(', '),
          disrupt ? [' · ', h('strong', null, 'connected peers drop and need new configs')] : null),
        h('div', { class: 'actions' },
          h('button', { type: 'button', class: 'btn', onClick: render }, 'Discard'),
          h('button', { type: 'button', class: 'btn primary', onClick: apply }, 'Apply')));
    };

    async function apply() {
      const body = {};
      for (const k of changed()) body[k] = draft[k];
      try {
        const res = await api('PATCH', '/server', body);
        await render();
        applied(res, 'Server settings applied');
        if (res.reissueNeeded) toast('Existing devices need a new config: endpoint, port or addresses changed.', false);
      } catch (e) { toast(e.message, true); }
    }

    const num = (k) => (e) => { draft[k] = e.target.value === '' ? 0 : Number(e.target.value); drawBar(); };
    const str = (k) => (e) => { draft[k] = e.target.value.trim(); drawBar(); };
    const bool = (k) => (e) => { draft[k] = e.target.checked; drawBar(); };
    const cd = (k, parse) => (e) => { draft.clientDefaults = { ...draft.clientDefaults, [k]: parse(e.target.value) }; drawBar(); };
    const restartTag = () => h('span', { class: 'tag' }, 'drops peers');
    const fieldEl = (id, label, input, hint) => h('div', { class: 'field' }, h('label', { htmlFor: id }, label), input, hint ? h('span', { class: 'hint' }, hint) : null);
    const cb = (k, label, hint) => h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: draft[k], onChange: bool(k) }), h('span', null, label, hint ? [h('br'), h('span', { class: 'hint' }, hint)] : null));

    const host = h('input', { id: 'host', class: 'mono', value: draft.endpoint, placeholder: 'vpn.example.net', onInput: str('endpoint') });
    const detectHint = h('span', { class: 'hint' }, 'A DNS name keeps configs valid when your IP changes');
    const detect = async () => {
      try {
        const r = await api('GET', '/server/detect-ip');
        detectHint.textContent = 'Detected public IP: ' + r.ip;
        if (!host.value) { host.value = r.ip; draft.endpoint = r.ip; drawBar(); }
      } catch (e) { toast(e.message, true); }
    };

    const dnsInput = h('input', { id: 'dns', class: 'mono', value: draft.clientDefaults.dns.join(', '), onInput: cd('dns', list) });
    const curDNS = draft.clientDefaults.dns.join(', ');
    const preset = h('select', { id: 'dp', onChange: (e) => { if (e.target.value) { dnsInput.value = e.target.value; dnsInput.dispatchEvent(new Event('input')); } } },
      DNS_PRESETS.map(([n, v]) => h('option', { value: v, selected: v === curDNS }, n)),
      h('option', { value: '', selected: !DNS_PRESETS.some(([, v]) => v === curDNS) }, 'Custom'));

    const rotate = async () => {
      if (!await confirmDialog({ title: 'Rotate the server key?', text: 'Every client config stops working until it is issued again. Use this only if the server key may have leaked.', ok: 'Rotate key', danger: true })) return;
      try { const res = await api('POST', '/server/rotate-key'); applied(res, 'Server key rotated'); render(); } catch (e) { toast(e.message, true); }
    };

    fill(wrap,
      h('div', null, h('h1', null, 'Server'), h('p', { class: 'sub' }, 'WireGuard interface, address plan, client defaults and firewall')),
      result,
      h('section', { class: 'card', 'aria-labelledby': 'hc' },
        h('h2', { id: 'hc' }, 'Health'),
        h('div', { style: { marginTop: '8px' } }, st.checks.map((c) => h('div', { class: 'chk' },
          h('span', { class: c.ok ? 'dot ok' : 'dot bad' }), h('span', { class: 'sr' }, c.ok ? 'OK: ' : 'Problem: '),
          h('b', null, c.name), h('span', { class: c.ok ? 'muted' : null }, c.detail))))),

      h('section', { class: 'card', 'aria-labelledby': 'if' },
        h('h2', { id: 'if' }, 'Interface'),
        h('p', { class: 'lead' }, 'Fields marked ', restartTag(), ' change what every client config contains: connected peers drop and need a new config.'),
        h('div', { class: 'grid' },
          fieldEl('ifn', 'Interface name', h('input', { id: 'ifn', class: 'mono', value: draft.interface, readOnly: true })),
          fieldEl('pt', ['Listen port', restartTag()], h('input', { id: 'pt', class: 'mono', inputMode: 'numeric', value: draft.listenPort, onInput: num('listenPort') }), 'UDP, 1–65535'),
          fieldEl('mtu', 'MTU', h('input', { id: 'mtu', class: 'mono', inputMode: 'numeric', value: draft.mtu, onInput: num('mtu') }), '1420 suits most links. Try 1280 on PPPoE or mobile'),
          fieldEl('v4', ['IPv4 network', restartTag()], h('input', { id: 'v4', class: 'mono', value: draft.ipv4, onInput: str('ipv4') }), 'The server uses the first address. Peers keep their host number'),
          fieldEl('v6', ['IPv6 network', restartTag()], h('input', { id: 'v6', class: 'mono', value: draft.ipv6, onInput: str('ipv6') })),
          h('div', { class: 'field', style: { justifyContent: 'flex-end' } }, cb('ipv6Enabled', 'Enable IPv6 in the tunnel')))),

      h('section', { class: 'card', 'aria-labelledby': 'ep' },
        h('h2', { id: 'ep' }, 'Public endpoint'),
        h('p', { class: 'lead' }, 'Where clients connect.'),
        h('div', { class: 'grid' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'host' }, 'Host'), h('div', { class: 'row' }, host, h('button', { type: 'button', class: 'btn', onClick: detect }, 'Detect IP')), detectHint),
          fieldEl('ept', 'Port seen by clients', h('input', { id: 'ept', class: 'mono', inputMode: 'numeric', value: draft.endpointPort || '', placeholder: String(draft.listenPort), onInput: num('endpointPort') }), 'Leave empty unless a router forwards a different port'))),

      h('section', { class: 'card', 'aria-labelledby': 'cd' },
        h('h2', { id: 'cd' }, 'Client defaults'),
        h('p', { class: 'lead' }, 'Used for new client configs and for peers set to "Server default". Existing devices pick up changes only after their config is issued again.'),
        h('div', { class: 'grid' },
          fieldEl('dp', 'DNS provider', preset),
          fieldEl('dns', 'DNS servers', dnsInput),
          fieldEl('al', 'AllowedIPs', h('input', { id: 'al', class: 'mono', value: draft.clientDefaults.allowedIPs.join(', '), onInput: cd('allowedIPs', list) }), '0.0.0.0/0, ::/0 is a full tunnel. Keep ::/0 even without server IPv6 to prevent leaks'),
          fieldEl('ka', 'Persistent keepalive', h('input', { id: 'ka', class: 'mono', inputMode: 'numeric', value: draft.clientDefaults.keepalive, onInput: cd('keepalive', (v) => Number(v) || 0) }), 'Seconds. 0 turns it off'))),

      h('section', { class: 'card', 'aria-labelledby': 'fw' },
        h('h2', { id: 'fw' }, 'Routing & firewall'),
        h('p', { class: 'lead' }, 'Rules live in their own nftables table. Other firewall rules aren\'t touched; if you also run ufw or firewalld, allow UDP ' + draft.listenPort + ' there.'),
        h('div', { class: 'grid' },
          fieldEl('up4', 'IPv4 uplink interface', h('input', { id: 'up4', class: 'mono', value: draft.uplinkV4, placeholder: 'auto: ' + (srv.detectedUplinkV4 || 'none found'), onInput: str('uplinkV4') })),
          fieldEl('up6', 'IPv6 uplink interface', h('input', { id: 'up6', class: 'mono', value: draft.uplinkV6, placeholder: 'auto: ' + (srv.detectedUplinkV6 || 'none found'), onInput: str('uplinkV6') })),
          cb('nat', 'Masquerade (NAT) peer traffic to the internet'),
          cb('peerToPeer', 'Allow peers to reach each other'),
          cb('lanAccess', 'Allow peers to reach the server\'s LAN', 'Private networks on the uplink interface'),
          cb('openPort', 'Accept UDP ' + draft.listenPort + ' in the input chain'))),

      h('section', { class: 'card', 'aria-labelledby': 'ky' },
        h('h2', { id: 'ky' }, 'Server key'),
        h('dl', { class: 'kv' },
          h('dt', null, 'Public key'), h('dd', { class: 'mono' }, srv.publicKey),
          h('dt', null, 'Created'), h('dd', null, fmtDate(srv.keyCreated))),
        h('div', { class: 'section' }, h('button', { type: 'button', class: 'btn danger', onClick: rotate }, 'Rotate server key…')),
        h('p', { class: 'hint' }, 'Every client config stops working until it\'s issued again.')),
      bar);
  }

  // ---------- settings ----------

  async function viewSettings(wrap) {
    if (!me.isAdmin) {
      fill(wrap, h('h1', null, 'Settings'), h('div', { class: 'notice' }, 'Only the admin account can change settings. API tokens cannot.'));
      return;
    }
    const [s, tk] = await Promise.all([api('GET', '/settings'), api('GET', '/tokens')]);
    let logLevelFilter = 'all';

    // admin account
    const uname = h('input', { id: 'u', value: s.adminUsername, autocomplete: 'username' });
    const cur = h('input', { id: 'pc', type: 'password', autocomplete: 'current-password' });
    const p1 = h('input', { id: 'p1', type: 'password', autocomplete: 'new-password', placeholder: 'At least 12 characters' });
    const p2 = h('input', { id: 'p2', type: 'password', autocomplete: 'new-password' });
    const accErr = h('p', { class: 'err-text', role: 'alert' });
    const savePw = async (e) => {
      e.preventDefault();
      accErr.textContent = '';
      try {
        if (uname.value.trim() !== s.adminUsername) {
          await api('PATCH', '/settings', { adminUsername: uname.value.trim() });
          toast('Username saved');
        }
        if (p1.value || p2.value || cur.value) {
          if (p1.value !== p2.value) throw new Error('The new passwords do not match');
          await api('POST', '/auth/password', { current: cur.value, new: p1.value });
          toast('Password changed. Please sign in again.');
          me = null;
          showLogin();
        }
      } catch (x) { accErr.textContent = x.message; }
    };

    // web interface
    const web = JSON.parse(JSON.stringify(s.web));
    const restartBox = h('div');
    const webErr = h('p', { class: 'err-text', role: 'alert' });
    const modeSel = h('select', { id: 'tls' },
      [['acme', 'Let\'s Encrypt (automatic)'], ['selfsigned', 'Self-signed certificate (generated)'], ['files', 'Certificate files'], ['off', 'Off: behind a reverse proxy']].map(([v, t]) => h('option', { value: v, selected: web.tls.mode === v }, t)));
    const fAcme = h('div', { class: 'grid' },
      h('div', { class: 'field' }, h('label', { htmlFor: 'dom' }, 'Domain'), h('input', { id: 'dom', class: 'mono', value: web.tls.domain || '', placeholder: 'vpn.example.net', onInput: (e) => { web.tls.domain = e.target.value.trim(); } }), h('span', { class: 'hint' }, 'Must point to this server. Port 443 (and 80 if set) must be reachable from the internet')),
      h('div', { class: 'field' }, h('label', { htmlFor: 'em' }, 'Email for Let\'s Encrypt'), h('input', { id: 'em', type: 'email', value: web.tls.email || '', onInput: (e) => { web.tls.email = e.target.value.trim(); } }), h('span', { class: 'hint' }, 'Optional. Used for expiry warnings')),
      h('label', { class: 'check' }, h('input', { type: 'checkbox', checked: !!web.tls.staging, onChange: (e) => { web.tls.staging = e.target.checked; } }), h('span', null, 'Use the staging CA', h('br'), h('span', { class: 'hint' }, 'For testing: certificates are not trusted by browsers'))));
    const fFiles = h('div', { class: 'grid' },
      h('div', { class: 'field' }, h('label', { htmlFor: 'cf' }, 'Certificate file'), h('input', { id: 'cf', class: 'mono', value: web.tls.certFile || '', placeholder: '/etc/ssl/vpn/fullchain.pem', onInput: (e) => { web.tls.certFile = e.target.value.trim(); } })),
      h('div', { class: 'field' }, h('label', { htmlFor: 'kf' }, 'Key file'), h('input', { id: 'kf', class: 'mono', value: web.tls.keyFile || '', placeholder: '/etc/ssl/vpn/privkey.pem', onInput: (e) => { web.tls.keyFile = e.target.value.trim(); } }), h('span', { class: 'hint' }, 'Readable by the service user. Reloaded when the file changes')));
    const syncMode = () => { web.tls.mode = modeSel.value; fAcme.hidden = modeSel.value !== 'acme'; fFiles.hidden = modeSel.value !== 'files'; };
    modeSel.addEventListener('change', syncMode);
    syncMode();
    const saveWeb = async (e) => {
      e.preventDefault();
      webErr.textContent = '';
      try {
        const res = await api('PATCH', '/settings', { web });
        if (res.restartRequired) {
          restartBox.replaceChildren(h('div', { class: 'notice' }, 'Saved. The web interface uses the new settings after a restart.',
            h('button', { type: 'button', class: 'btn small', onClick: restartNow }, 'Restart now')));
        } else toast('Saved');
      } catch (x) { webErr.textContent = x.message; }
    };
    const restartNow = async () => {
      try { await api('POST', '/restart'); } catch { /* the connection may drop */ }
      toast('Restarting… the page reloads in a few seconds.');
      setTimeout(() => location.reload(), 4000);
    };

    // tokens
    const tbody = h('tbody');
    const drawTokens = (tokens) => tbody.replaceChildren(...(tokens.length ? tokens.map((t) => h('tr', null,
      h('td', null, t.name),
      h('td', null, h('span', { class: 'badge' }, t.scope === 'ro' ? 'Read only' : 'Full access')),
      h('td', null, fmtDate(t.created)),
      h('td', null, t.lastUsed ? ago(t.lastUsed.at) + ' · ' + t.lastUsed.ip : 'Not since restart'),
      h('td', { class: 'num' }, h('button', { type: 'button', class: 'btn danger small', onClick: () => revoke(t) }, 'Revoke'))))
      : [h('tr', null, h('td', { colspan: '5', class: 'muted' }, 'No tokens yet.'))]));
    drawTokens(tk.tokens);
    const reloadTokens = async () => drawTokens((await api('GET', '/tokens')).tokens);
    const revoke = async (t) => {
      if (!await confirmDialog({ title: 'Revoke ' + t.name + '?', text: 'Apps using this token are signed out immediately.', ok: 'Revoke', danger: true })) return;
      try { await api('DELETE', '/tokens/' + t.id); toast('Revoked ' + t.name); reloadTokens(); } catch (e) { toast(e.message, true); }
    };
    const newToken = () => {
      const nm = h('input', { id: 'tn', value: 'iPhone app' });
      const sc = h('select', { id: 'ts' }, h('option', { value: 'rw' }, 'Full access'), h('option', { value: 'ro' }, 'Read only'));
      const e = h('p', { class: 'err-text' });
      const d = dialog((close) => {
        const body = h('form', { class: 'dlg', onSubmit: async (ev) => {
          ev.preventDefault();
          try {
            const r = await api('POST', '/tokens', { name: nm.value, scope: sc.value });
            body.replaceChildren(
              h('h2', null, 'Scan with the iOS app'),
              h('div', { class: 'notice' }, 'The token is shown only now. Only a hash is stored on the server.'),
              h('div', { class: 'qrrow' },
                h('img', { class: 'qr', src: r.qr, alt: 'Pairing QR code' }),
                h('div', { class: 'col' },
                  h('button', { type: 'button', class: 'btn', onClick: () => copy(r.pairing) }, 'Copy pairing code'),
                  h('button', { type: 'button', class: 'btn', onClick: () => copy(r.token) }, 'Copy token'))),
              h('pre', { class: 'code' }, r.token),
              h('div', { class: 'foot' }, h('button', { type: 'button', class: 'btn primary', onClick: close }, 'Done')));
            reloadTokens();
          } catch (x) { e.textContent = x.message; }
        } },
        h('h2', null, 'New API token'),
        h('p', null, 'For the iOS app or scripts. The pairing QR code holds the server address, the token and the certificate fingerprint.'),
        h('div', { class: 'field' }, h('label', { htmlFor: 'tn' }, 'Name'), nm),
        h('div', { class: 'field' }, h('label', { htmlFor: 'ts' }, 'Access'), sc),
        e,
        h('div', { class: 'foot' }, h('button', { type: 'button', class: 'btn', onClick: close }, 'Cancel'), h('button', { type: 'submit', class: 'btn primary' }, 'Create token')));
        return body;
      });
      return d;
    };

    // logs
    const logBox = h('pre', { class: 'log', tabindex: '0', 'aria-label': 'Log lines, newest first' });
    const logPills = h('div', { class: 'pills', role: 'group', 'aria-label': 'Level filter' });
    const drawLogs = async () => {
      logPills.replaceChildren(...[['all', 'All'], ['info', 'Info'], ['warn', 'Warn'], ['error', 'Error']].map(([k, t]) =>
        h('button', { type: 'button', class: logLevelFilter === k ? 'pill on' : 'pill', 'aria-pressed': String(logLevelFilter === k), onClick: () => { logLevelFilter = k; drawLogs(); } }, t)));
      try {
        const r = await api('GET', '/logs?limit=200&level=' + logLevelFilter);
        logBox.textContent = r.lines.length ? r.lines.map(fmtLogLine).join('\n') : 'No entries at this level.';
      } catch (e) { logBox.textContent = e.message; }
    };
    const levelSel = h('select', { id: 'lv', onChange: async (e) => {
      try { await api('PATCH', '/settings', { log: { ...s.log, level: e.target.value } }); s.log.level = e.target.value; toast('Log level: ' + e.target.value); } catch (x) { toast(x.message, true); }
    } }, ['debug', 'info', 'warn', 'error'].map((l) => h('option', { value: l, selected: s.log.level === l }, l)));

    // data retention
    const presetSelect = (id, value, presets, unit) => {
      const opts = presets.some(([v]) => v === value) ? presets : [...presets, [value, value + ' ' + unit]].sort((a, b) => a[0] - b[0]);
      return h('select', { id }, opts.map(([v, t]) => h('option', { value: String(v), selected: v === value }, t)));
    };
    const logSize = h('input', { id: 'rs', type: 'number', min: '1', max: '1000', value: s.log.maxSizeMB, inputMode: 'numeric' });
    const logFiles = h('input', { id: 'rf', type: 'number', min: '1', max: '100', value: s.log.maxFiles, inputMode: 'numeric' });
    const hourly = presetSelect('rh', s.stats.hourlyHours, [[24, '1 day'], [48, '2 days'], [168, '7 days'], [336, '14 days'], [744, '31 days']], 'hours');
    const daily = presetSelect('rd', s.stats.dailyDays, [[30, '30 days'], [90, '90 days'], [180, '6 months'], [400, '13 months'], [730, '2 years'], [1825, '5 years'], [3660, '10 years']], 'days');
    const geo = h('input', { type: 'checkbox', checked: s.stats.geoip !== false });
    const geoStatus = s.geo && s.geo.updated ? 'Database from ' + fmtDate(s.geo.updated) + '.' : 'Not downloaded yet.';
    const diskHint = h('span', { class: 'hint' });
    const drawDiskHint = () => {
      const mb = Number(logSize.value) * (Number(logFiles.value) + 1);
      diskHint.textContent = mb > 0 ? 'The log uses up to ' + mb + ' MB on disk (current file plus kept files).' : '';
    };
    logSize.addEventListener('input', drawDiskHint);
    logFiles.addEventListener('input', drawDiskHint);
    drawDiskHint();
    const retErr = h('p', { class: 'err-text', role: 'alert' });
    const saveRetention = async (e) => {
      e.preventDefault();
      retErr.textContent = '';
      const next = { maxSizeMB: Number(logSize.value), maxFiles: Number(logFiles.value), hourlyHours: Number(hourly.value), dailyDays: Number(daily.value) };
      const shrinks = next.maxFiles < s.log.maxFiles || next.hourlyHours < s.stats.hourlyHours || next.dailyDays < s.stats.dailyDays;
      if (shrinks && !await confirmDialog({ title: 'Delete older data?', text: 'The new limits are lower: older log files and traffic history beyond them are deleted. This cannot be undone.', ok: 'Save and delete', danger: true })) return;
      try {
        await api('PATCH', '/settings', {
          log: { ...s.log, maxSizeMB: next.maxSizeMB, maxFiles: next.maxFiles },
          stats: { hourlyHours: next.hourlyHours, dailyDays: next.dailyDays, geoip: geo.checked },
        });
        toast('Retention saved');
        render();
      } catch (x) { retErr.textContent = x.message; }
    };

    // backup
    const restoreInput = h('input', { type: 'file', accept: 'application/json,.json', hidden: true, onChange: async (e) => {
      const f = e.target.files[0];
      e.target.value = '';
      if (!f) return;
      let cfg;
      try { cfg = JSON.parse(await f.text()); } catch { toast('This file is not valid JSON', true); return; }
      if (!await confirmDialog({ title: 'Restore this backup?', text: 'All current settings, peers and tokens are replaced by the ones in ' + f.name + '.', ok: 'Restore', danger: true })) return;
      try {
        const res = await api('POST', '/restore', cfg);
        applied(res, 'Backup restored');
        restartBox.replaceChildren(h('div', { class: 'notice' }, 'Backup restored. Restart to apply web interface settings.', h('button', { type: 'button', class: 'btn small', onClick: restartNow }, 'Restart now')));
      } catch (x) { toast(x.message, true); }
    } });

    fill(wrap,
      h('div', null, h('h1', null, 'Settings'), h('p', { class: 'sub' }, 'Web interface, API access for the iOS app, logs, data retention and backups')),
      restartBox,

      h('form', { class: 'card', onSubmit: savePw, 'aria-labelledby': 'acc' },
        h('h2', { id: 'acc' }, 'Admin account'),
        h('p', { class: 'lead' }, 'One admin account. The iOS app signs in with an API token, not this password.'),
        h('div', { class: 'grid' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'u' }, 'Username'), uname),
          h('div', { class: 'field' }, h('label', { htmlFor: 'pc' }, 'Current password'), cur),
          h('div', { class: 'field' }, h('label', { htmlFor: 'p1' }, 'New password'), p1),
          h('div', { class: 'field' }, h('label', { htmlFor: 'p2' }, 'Confirm new password'), p2)),
        accErr,
        h('div', { class: 'formfoot' }, h('button', { type: 'submit', class: 'btn' }, 'Save account'))),

      h('form', { class: 'card', onSubmit: saveWeb, 'aria-labelledby': 'web' },
        h('h2', { id: 'web' }, 'Web interface'),
        h('p', { class: 'lead' }, 'Changes take effect after the service restarts.'),
        h('div', { class: 'grid' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'la' }, 'Listen address'), h('input', { id: 'la', class: 'mono', value: web.listen, onInput: (e) => { web.listen = e.target.value.trim(); } })),
          h('div', { class: 'field' }, h('label', { htmlFor: 'hl' }, 'HTTP listen address'), h('input', { id: 'hl', class: 'mono', value: web.httpListen, placeholder: 'off', onInput: (e) => { web.httpListen = e.target.value.trim(); } }), h('span', { class: 'hint' }, 'Redirects to HTTPS and answers Let\'s Encrypt http-01 checks. Empty turns it off')),
          h('div', { class: 'field' }, h('label', { htmlFor: 'tls' }, 'HTTPS'), modeSel),
          h('div', { class: 'field' }, h('label', { htmlFor: 'st' }, 'Session length'), h('select', { id: 'st', onChange: (e) => { web.sessionHours = Number(e.target.value); } },
            [[1, '1 hour'], [12, '12 hours'], [24, '1 day'], [168, '7 days']].map(([v, t]) => h('option', { value: String(v), selected: web.sessionHours === v }, t)))),
          s.fingerprint ? h('div', { class: 'field' }, h('label', { htmlFor: 'fp' }, 'Certificate fingerprint (SHA-256)'), h('input', { id: 'fp', class: 'mono', value: s.fingerprint, readOnly: true }), h('span', { class: 'hint' }, 'The iOS app pins this when pairing')) : null),
        h('div', { class: 'section' }, fAcme, fFiles),
        webErr,
        h('div', { class: 'formfoot' }, h('button', { type: 'submit', class: 'btn primary' }, 'Save'))),

      h('section', { class: 'card', 'aria-labelledby': 'api' },
        h('div', { class: 'cardhead' },
          h('div', null, h('h2', { id: 'api' }, 'API tokens'), h('p', { class: 'lead', style: { marginBottom: '0' } }, 'For the iOS app and scripts. A token appears once when you create it, and only a hash is stored.')),
          h('button', { type: 'button', class: 'btn primary', onClick: newToken }, 'Pair iOS app')),
        h('div', { class: 'tbl section' }, h('table', { class: 'narrow' },
          h('thead', null, h('tr', null, h('th', null, 'Name'), h('th', null, 'Access'), h('th', null, 'Created'), h('th', null, 'Last used'), h('th', null, h('span', { class: 'sr' }, 'Actions')))),
          tbody))),

      h('section', { class: 'card', 'aria-labelledby': 'lg' },
        h('div', { class: 'cardhead' },
          h('div', null, h('h2', { id: 'lg' }, 'Log'), h('p', { class: 'lead', style: { marginBottom: '0' } }, h('span', { class: 'mono' }, s.logPath), ' · rotates at ' + s.log.maxSizeMB + ' MB, keeps ' + s.log.maxFiles + ' files')),
          logPills),
        h('div', { class: 'section' }, logBox),
        h('div', { class: 'grid section' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'lv' }, 'Log level'), levelSel),
          h('div', { class: 'field', style: { justifyContent: 'flex-end' } }, h('a', { class: 'btn', href: '/api/v1/logs/download' }, 'Download log')))),

      h('form', { class: 'card', onSubmit: saveRetention, 'aria-labelledby': 'ret' },
        h('h2', { id: 'ret' }, 'Data retention'),
        h('p', { class: 'lead' }, 'How much log and traffic history is kept. Changes apply immediately, without a restart.'),
        h('div', { class: 'grid' },
          h('div', { class: 'field' }, h('label', { htmlFor: 'rs' }, 'Log file size (MB)'), logSize, h('span', { class: 'hint' }, 'The log starts a new file at this size. 1–1000')),
          h('div', { class: 'field' }, h('label', { htmlFor: 'rf' }, 'Old log files kept'), logFiles, diskHint),
          h('div', { class: 'field' }, h('label', { htmlFor: 'rh' }, 'Hourly traffic history'), hourly, h('span', { class: 'hint' }, 'Used by the 24-hour charts')),
          h('div', { class: 'field' }, h('label', { htmlFor: 'rd' }, 'Daily traffic history'), daily, h('span', { class: 'hint' }, 'Used by the 7- and 30-day charts and the connection history. All-time totals are always kept'))),
        h('label', { class: 'check section' }, geo, h('span', null, 'Show country and network of peer addresses', h('br'),
          h('span', { class: 'hint' }, 'Downloads the free DB-IP Lite databases (about 20 MB) once a month and looks addresses up on this server only. ' + geoStatus))),
        retErr,
        h('div', { class: 'formfoot' }, h('button', { type: 'submit', class: 'btn primary' }, 'Save retention'))),

      h('section', { class: 'card', 'aria-labelledby': 'bk' },
        h('h2', { id: 'bk' }, 'Backup & restore'),
        h('p', { class: 'lead' }, 'A backup is a copy of config.json with server key, peers, tokens and settings. Keep it safe: it contains the server\'s private key.'),
        h('div', { class: 'actions' },
          h('a', { class: 'btn', href: '/api/v1/backup' }, 'Download backup'),
          h('button', { type: 'button', class: 'btn', onClick: () => restoreInput.click() }, 'Restore from file…'),
          restoreInput)));
    await drawLogs();
  }

  render();
})();
