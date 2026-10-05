'use strict';
// The page a setup link opens. It works without signing in: the token in
// the URL is the credential. The config exists only in this page's memory;
// leaving the page loses it, as the link works once.
(() => {
  const app = document.getElementById('app');
  const token = location.pathname.split('/').pop();

  function h(tag, props, ...kids) {
    const el = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.startsWith('on')) el.addEventListener(k.slice(2).toLowerCase(), v);
      else if (['value', 'hidden', 'htmlFor', 'disabled', 'src', 'alt', 'href', 'type', 'id', 'autocomplete', 'inputMode', 'maxLength', 'required'].includes(k)) el[k] = v;
      else el.setAttribute(k, v === true ? '' : v);
    }
    for (const c of kids.flat(Infinity)) if (c != null && c !== false) el.append(c instanceof Node ? c : String(c));
    return el;
  }

  // Same drawing as favicon.svg.
  // ext opens an outside page in a new tab, marked with ↗ as in the app.
  const ext = (href, text) => h('a', { class: 'ext', href, target: '_blank', rel: 'noopener' }, text,
    h('span', { class: 'ar', 'aria-hidden': 'true' }, '↗'), h('span', { class: 'sr' }, ' (opens in a new tab)'));

  function logo(size, plain) {
    const s = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    for (const [k, v] of Object.entries({ width: size, height: size, viewBox: '0 0 64 64', 'aria-hidden': 'true' })) s.setAttribute(k, v);
    s.innerHTML = (plain ? '' : '<rect width="64" height="64" rx="14" fill="#1b1b1d"/><rect x="0.5" y="0.5" width="63" height="63" rx="13.5" fill="none" stroke="#fff" stroke-opacity="0.2"/>') +
      '<g transform="translate(32 34) scale(0.9) translate(-32 -27)"><path d="M22 21C17 17 15 11 16 5c3 5 7 8 12 11zM42 21c5-4 7-10 6-16-3 5-7 8-12 11z" fill="#fff"/><path d="M18 46V29c0-9 6-14 14-14s14 5 14 14v17l-4.5-5-4.5 8-5-6-5 6-4.5-8z" fill="#fff"/>' +
      (plain ? '' : '<path d="M20 27l10 4.5-1.5 2.5-6.5-1.5zM44 27l-10 4.5 1.5 2.5 6.5-1.5z" fill="#c8372d"/>') + '</g>';
    return s;
  }

  const fmtExpiry = (iso) => new Date(iso).toLocaleString(undefined, { weekday: 'short', day: 'numeric', month: 'short', hour: '2-digit', minute: '2-digit' });

  async function call(method, body) {
    const opt = { method, headers: {} };
    if (body) {
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    const r = await fetch('/api/v1/setup/' + encodeURIComponent(token), opt);
    let data = {};
    try { data = await r.json(); } catch { /* empty */ }
    return { status: r.status, data };
  }

  function show(...kids) { app.replaceChildren(h('div', { class: 'setupbox' }, kids)); }

  function invalid() {
    app.replaceChildren(h('div', { class: 'setupbox center' },
      h('span', { class: 'ghost' }, logo(72, true)),
      h('h1', null, 'This link isn\'t valid'),
      h('p', null, 'It was already used, it expired, or it was revoked. Ask whoever sent it for a new one.'),
      h('p', { class: 'loginfoot' }, 'GHOSTWIRE')));
  }

  function start(info) {
    const err = h('p', { class: 'err-text', role: 'alert' });
    const pin = info.pinRequired
      ? h('input', { id: 'pin', class: 'pin', inputMode: 'numeric', autocomplete: 'one-time-code', maxLength: 8, required: true })
      : null;
    const btn = h('button', { type: 'submit', class: 'btn primary' }, info.pinRequired ? 'Continue' : 'Get my VPN profile');
    const form = h('form', { class: 'loginform', onSubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      btn.disabled = true;
      try {
        const { status, data } = await call('POST', { pin: pin ? pin.value.trim() : '' });
        if (status === 200) return ready(data);
        if (status === 404) return invalid();
        err.textContent = data.error || 'Something went wrong. Try again.';
        if (pin) pin.select();
      } catch {
        err.textContent = 'No connection to the server. Try again.';
      }
      btn.disabled = false;
    } },
    pin ? h('div', { class: 'field' }, h('label', { htmlFor: 'pin' }, 'PIN'), pin) : null,
    err, btn);
    app.replaceChildren(h('div', { class: 'setupbox' },
      h('div', { class: 'brand stack' }, logo(56), h('div', { class: 'wm' }, 'GHOSTWIRE')),
      h('div', { class: 'center' },
        h('h1', null, 'Set up your VPN'),
        h('p', null, 'This link adds the VPN profile ', h('strong', { class: 'mono' }, info.name), ' to your device.',
          info.pinRequired ? ' Enter the PIN you were given.' : '')),
      form,
      h('p', { class: 'note' }, 'The link works once and expires ' + fmtExpiry(info.expires) + '.')));
    (pin || btn).focus();
  }

  function ready(res) {
    const file = res.name + '.conf';
    const download = () => {
      const url = URL.createObjectURL(new Blob([res.config], { type: 'application/octet-stream' }));
      const a = h('a', { href: url, download: file });
      document.body.append(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    };
    const qr = h('img', { class: 'qr', src: res.qr, alt: 'QR code of the VPN profile ' + res.name, hidden: true });
    const qrBtn = h('button', { type: 'button', class: 'btn', onClick: () => { qr.hidden = !qr.hidden; qrBtn.textContent = qr.hidden ? 'Show QR code' : 'Hide QR code'; } }, 'Show QR code');
    const step = (n, title, ...body) => h('li', null, h('span', { class: 'num' }, String(n)), h('div', null, h('strong', null, title), body));
    // Leaving the page loses the only copy of the private key.
    window.addEventListener('beforeunload', (e) => e.preventDefault());
    show(
      h('div', { class: 'brand' }, logo(32), h('div', { class: 'wm' }, 'GHOSTWIRE')),
      h('h1', null, 'Your VPN profile is ready'),
      h('div', { class: 'notice' }, 'Save it now. This page can\'t be opened again: the private key exists only here and isn\'t stored anywhere.'),
      h('ol', { class: 'steps' },
        step(1, 'Install WireGuard',
          h('p', null, ext('https://apps.apple.com/app/wireguard/id1441195209', 'App Store'), ' · ',
            ext('https://play.google.com/store/apps/details?id=com.wireguard.android', 'Google Play'), ' · ',
            ext('https://www.wireguard.com/install/', 'Other systems'))),
        step(2, 'Add the profile',
          h('button', { type: 'button', class: 'btn primary', onClick: download }, 'Download ' + file),
          h('p', null, 'Open the downloaded file with WireGuard, or in WireGuard tap + and choose “Create from file”.')),
        step(3, 'Setting up another device?',
          qrBtn, qr,
          h('p', null, 'Opened this on a computer? Scan the QR code with WireGuard on your phone.'))));
  }

  (async () => {
    try {
      const { status, data } = await call('GET');
      if (status === 200) start(data);
      else invalid();
    } catch {
      show(h('h1', null, 'No connection'), h('p', null, 'The server can\'t be reached. Reload the page to try again.'));
    }
  })();
})();
