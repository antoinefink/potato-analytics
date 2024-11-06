(function () {
  var currentPath = null;
  var dbPromise = null;

  // Open or create IndexedDB
  function openDB() {
    if (!dbPromise) {
      dbPromise = new Promise(function (resolve, reject) {
        var request = indexedDB.open('trackingDB', 1);
        request.onupgradeneeded = function (event) {
          var db = event.target.result;
          if (!db.objectStoreNames.contains('trackedUrls')) {
            db.createObjectStore('trackedUrls', { keyPath: 'url' });
          }
        };
        request.onsuccess = function (event) {
          resolve(event.target.result);
        };
        request.onerror = function (event) {
          reject(event.target.error);
        };
      });
    }
    return dbPromise;
  }

  // Save URL with timestamp to IndexedDB
  function saveUrl(url) {
    return openDB().then(function (db) {
      return new Promise(function (resolve, reject) {
        var transaction = db.transaction('trackedUrls', 'readwrite');
        var store = transaction.objectStore('trackedUrls');
        store.put({ url: url, timestamp: Date.now() });
        transaction.oncomplete = function () {
          resolve();
        };
        transaction.onerror = function (event) {
          reject(event.target.error);
        };
      });
    });
  }

  // Check if the URL has been tracked today
  function shouldTrackUrl(url) {
    return openDB().then(function (db) {
      return new Promise(function (resolve) {
        var transaction = db.transaction('trackedUrls', 'readonly');
        var store = transaction.objectStore('trackedUrls');
        var request = store.get(url);
        request.onsuccess = function (event) {
          var result = event.target.result;
          if (result) {
            var oneDay = 24 * 60 * 60 * 1000;
            if (Date.now() - result.timestamp < oneDay) {
              resolve(false); // Already tracked within the last day
              return;
            }
          }
          resolve(true); // Not tracked today
        };
        request.onerror = function () {
          resolve(true); // If there's an error, assume we should track
        };
      });
    });
  }

  var trackEvent = function (eventType) {
    shouldTrackUrl(window.location.pathname).then(function (shouldTrack) {
      if (shouldTrack) {
        try {
          navigator.sendBeacon('%s', new URLSearchParams({
            url: window.location.href,
            eventType: eventType // Add eventType to the tracked data
          }));
          saveUrl(window.location.pathname);
        } catch (e) { }
      }
    });
  };

  function trackPageview() {
    if (currentPath !== window.location.pathname) {
      currentPath = window.location.pathname;
      trackEvent('pageview');
    }
  }

  // Handle initial pageview based on document visibility
  if (document.visibilityState === "prerender") {
    document.addEventListener("visibilitychange", function () {
      if (document.visibilityState === "visible") {
        trackPageview();
      }
    });
  } else {
    trackPageview();
  }

  // Handle navigation events
  var pushState = history.pushState;
  var replaceState = history.replaceState;

  history.pushState = function () {
    pushState.apply(this, arguments);
    trackPageview();
  };

  history.replaceState = function () {
    replaceState.apply(this, arguments);
    trackPageview();
  };

  window.addEventListener('popstate', trackPageview);
})();
