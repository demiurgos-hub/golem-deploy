/*
 * Golem Deploy: Inspector boot script, served at /_golem-deploy/boot.js and
 * inserted at the top of every Inspector page. It attaches the session key
 * this tab got from its entry link (sessionStorage of the Inspector's
 * origin) to the page's own requests: same-origin fetch calls carry
 * "Authorization: Bearer <key>", same-origin WebSockets offer the
 * subprotocols "golem-deploy.inspector" and "golem-deploy.key.<key>".
 * Without a key, or when the daemon says the session ended, the tab goes to
 * /_golem-deploy/ended. Plain ES5, no dependencies, ASCII only.
 */
(function (w) {
  'use strict';

  var STORE = 'golem_deploy_inspector_key';
  var ENDED = '/_golem-deploy/ended';
  var MARKER = 'golem-deploy.inspector';
  var KEY_PROTOCOL = 'golem-deploy.key.';
  var loc = w.location;
  var key = null;

  try {
    key = w.sessionStorage.getItem(STORE);
  } catch (e) {
    key = null;
  }

  function end() {
    try {
      w.sessionStorage.removeItem(STORE);
    } catch (e) {
      // Storage is not available: nothing to remove.
    }
    loc.replace(ENDED);
  }

  if (typeof key !== 'string' || !/^[A-Za-z0-9_-]{43}$/.test(key)) {
    try {
      w.stop();
    } catch (e) {
      // Leaving is enough.
    }
    end();
    return;
  }

  var NativeURL = w.URL;
  var NativeRequest = typeof w.Request === 'function' ? w.Request : null;

  // resolve returns the string text as an absolute URL, resolved the way
  // the browser resolves fetch and WebSocket URLs (against the document's
  // base URL, so a <base> element counts), or null.
  function resolve(text) {
    var doc = w.document;
    var base = doc && typeof doc.baseURI === 'string' && doc.baseURI !== '' ? doc.baseURI : loc.href;
    try {
      return new NativeURL(text, base);
    } catch (e) {
      return null;
    }
  }

  // wsHref is the WebSocket form of an http or https URL.
  function wsHref(u) {
    var href = u.href;
    if (u.protocol === 'http:') {
      return 'ws:' + href.slice(5);
    }
    if (u.protocol === 'https:') {
      return 'wss:' + href.slice(6);
    }
    return href;
  }

  // sameOrigin reports whether u (http, https, ws or wss) is this page's
  // own origin: scheme, host and port.
  function sameOrigin(u) {
    if (u === null) {
      return false;
    }
    var scheme = u.protocol;
    if (scheme === 'ws:') {
      scheme = 'http:';
    } else if (scheme === 'wss:') {
      scheme = 'https:';
    }
    return scheme + '//' + u.host === loc.protocol + '//' + loc.host;
  }

  // The URL or the Request that is checked is exactly what reaches the
  // native fetch and WebSocket, for every origin: the page's value is
  // turned into a string once and never passed on again.
  var origFetch = w.fetch;
  if (typeof origFetch === 'function') {
    w.fetch = function (input, init) {
      var req = null;
      var text;
      if (NativeRequest !== null && input instanceof NativeRequest) {
        // A copy made by the native constructor: its URL is the one the
        // browser uses, whatever the page's object reports.
        try {
          req = new NativeRequest(input);
        } catch (e) {
          // The native fetch fails the same way for this Request.
          return Promise.reject(e);
        }
        text = req.url;
      } else {
        try {
          text = String(input);
        } catch (e) {
          return Promise.reject(e);
        }
      }
      var target = resolve(text);
      if (!sameOrigin(target)) {
        var plain = req !== null ? req : target !== null ? target.href : text;
        return init === undefined ? origFetch.call(w, plain) : origFetch.call(w, plain, init);
      }
      var opts = {};
      var k;
      if (init) {
        for (k in init) {
          if (Object.prototype.hasOwnProperty.call(init, k)) {
            opts[k] = init[k];
          }
        }
      }
      // Headers in init replace those of a Request input, as in fetch.
      var headers = new w.Headers(init && init.headers !== undefined ? init.headers : req ? req.headers : undefined);
      headers.set('Authorization', 'Bearer ' + key);
      opts.headers = headers;
      var method = String(opts.method || (req ? req.method : 'GET')).toUpperCase();
      var logout = target.pathname === '/auth/logout' && method === 'POST';
      return origFetch.call(w, req !== null ? req : target.href, opts).then(function (resp) {
        if ((resp.status === 401 && resp.headers.get('X-Golem-Deploy-Session') === 'ended') || (logout && resp.ok)) {
          end();
        }
        return resp;
      });
    };
  }

  var OrigWebSocket = w.WebSocket;
  if (typeof OrigWebSocket === 'function') {
    var Wrapped = function WebSocket(url, protocols) {
      var text = String(url);
      var target = resolve(text);
      var href = target !== null ? wsHref(target) : text;
      if (!sameOrigin(target)) {
        return arguments.length > 1 ? new OrigWebSocket(href, protocols) : new OrigWebSocket(href);
      }
      var list = [];
      if (typeof protocols === 'string') {
        list.push(protocols);
      } else if (protocols !== undefined && protocols !== null) {
        list = Array.prototype.slice.call(protocols);
      }
      list.push(MARKER, KEY_PROTOCOL + key);
      return new OrigWebSocket(href, list);
    };
    Wrapped.prototype = OrigWebSocket.prototype;
    var names = ['CONNECTING', 'OPEN', 'CLOSING', 'CLOSED'];
    for (var i = 0; i < names.length; i++) {
      Wrapped[names[i]] = OrigWebSocket[names[i]];
    }
    w.WebSocket = Wrapped;
  }
}(window));
