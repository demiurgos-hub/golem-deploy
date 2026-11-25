/*
 * Golem Deploy: Inspector entry script, served at /_golem-deploy/enter.js
 * for the entry page /_golem-deploy/enter#t=<ticket>. It takes the ticket
 * from the fragment, removes the fragment from the address bar and the
 * history, exchanges the ticket for a session key at
 * POST /_golem-deploy/session, keeps the key in this tab's sessionStorage
 * and opens the Inspector. Plain ES5, no dependencies, ASCII only.
 */
(function (w) {
  'use strict';

  var STORE = 'golem_deploy_inspector_key';
  var doc = w.document;
  var loc = w.location;
  var messages = {
    link_invalid: ['Inspector link not valid',
      'This Inspector link was already used or has expired. Open the Inspector again from Golem Deploy.'],
    account_invalid: ['Inspector link not valid',
      'The account of this Inspector link can no longer open it. Sign in to Golem Deploy and open the Inspector again.'],
    not_running: ['Inspector not running',
      'The Inspector is not running. Check its state in Golem Deploy.'],
    too_many_attempts: ['Too many attempts',
      'Too many Inspector links failed from this address. Wait a few minutes, then open the Inspector again from Golem Deploy.'],
    storage: ['Inspector unavailable',
      'This browser does not let the Inspector keep its session in this tab. Allow site data for this address, then open the Inspector again from Golem Deploy.'],
    unavailable: ['Inspector unavailable',
      'The Inspector could not be opened right now. Try again in a moment from Golem Deploy.']
  };

  function show(code) {
    var m = Object.prototype.hasOwnProperty.call(messages, code) ? messages[code] : messages.unavailable;
    doc.title = m[0];
    doc.getElementById('title').textContent = m[0];
    doc.getElementById('text').textContent = m[1];
  }

  var match = /^#t=([A-Za-z0-9_-]{43})$/.exec(loc.hash || '');
  try {
    w.history.replaceState(null, '', loc.pathname + loc.search);
  } catch (e) {
    // Without the History API the fragment stays; the ticket is used once.
  }
  if (!match) {
    show('link_invalid');
    return;
  }

  w.fetch('/_golem-deploy/session', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ ticket: match[1] }),
    credentials: 'omit',
    cache: 'no-store'
  }).then(function (resp) {
    return resp.json().then(function (body) {
      return { ok: resp.ok, body: body };
    }, function () {
      return { ok: false, body: null };
    });
  }).then(function (r) {
    var body = r.body !== null && typeof r.body === 'object' ? r.body : {};
    if (!r.ok || typeof body.key !== 'string' || body.key === '') {
      show(typeof body.error === 'string' ? body.error : 'unavailable');
      return;
    }
    try {
      w.sessionStorage.setItem(STORE, body.key);
    } catch (e) {
      show('storage');
      return;
    }
    var dest = '/';
    if (typeof body.target_id === 'string' && body.target_id !== '') {
      dest = '/?targetId=' + encodeURIComponent(body.target_id);
    }
    loc.replace(dest);
  }, function () {
    show('unavailable');
  });
}(window));
