// A service worker that does nothing, on purpose. It has no fetch handler, so
// it never sees a request and caches nothing: labels are private, and the
// page is useless offline anyway.
//
// It exists for one reason. When the Android share sheet hands a file to the
// installed app and the site has no service worker at all, Chrome first loads
// the start page and waits up to 5 seconds for one to appear before it sends
// the share. With this registered, Chrome sees a worker without a fetch
// handler and sends the share to /share straight away.
'use strict';

self.addEventListener('install', () => self.skipWaiting());
