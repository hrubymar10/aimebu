// aimebu – room-based frontend
// Vanilla JS, no build tools, no frameworks.

(function () {
  'use strict';

  // ── State ────────────────────────────────────────────────────────

  let rooms = [];              // Room[] — all rooms from server
  let activeRoomID = null;     // currently viewed room ID
  let messages = {};           // { roomID: Message[] }
  let agents = [];             // Agent[]
  // First-time visitors get 'user' as a placeholder; init() will try to
  // replace it with $AIMEBU_NAME from the server before registration.
  let agentID = localStorage.getItem('aimebu_agent_id') || 'user';
  let agentIDFromStorage = localStorage.getItem('aimebu_agent_id') !== null;
  let registered = false; // true after a successful POST /agents for current agentID
  let ws = null;               // WebSocket connection
  let wsReconnectTimer = null;
  let wsReconnectAttempts = 0;
  let prevSubscribedRoom = null; // room we're currently subscribed to via WS
  let lastMessagePreview = {}; // { roomID: string } — for sidebar preview
  let unreadCounts = {};       // { roomID: number } — unread per room for current agent
  let readCursors = {};        // { roomID: int64 as number } — cursor per room
  // presence[roomID][agentID] = { cursor, waiting } — per-room per-agent
  // read cursor + live-listening flag. Populated by GET /rooms/{id}
  // (members_presence) and kept in sync by WS `presence` events.
  // The special bucket presence['*'][agentID] carries agent-wide waits
  // (bus_wait without a room filter) — those apply to every room the agent
  // is in.
  let presence = {};

  // ── DOM refs ─────────────────────────────────────────────────────

  const $ = (sel) => document.querySelector(sel);
  const agentIDInput = $('#agent-id-input');
  const connectionStatus = $('#connection-status');
  const statusText = connectionStatus.querySelector('.status-text');
  const settingsBtn = $('#settings-btn');
  const settingsDropdown = $('#settings-dropdown');
  const clearAllBtn = $('#clear-all-btn');

  const joinRoomInput = $('#join-room-input');
  const joinRoomBtn = $('#join-room-btn');
  const roomListEl = $('#room-list');
  const dmListEl = $('#dm-list');

  const noRoomView = $('#no-room-view');
  const roomView = $('#room-view');
  const roomIconEl = $('#room-icon');
  const roomNameEl = $('#room-name');
  const roomMemberCount = $('#room-member-count');
  const roomMemberAvatars = $('#room-member-avatars');
  const leaveRoomBtn = $('#leave-room-btn');
  const messageListEl = $('#message-list');
  const sendForm = $('#send-form');
  const msgBodyInput = $('#msg-body');

  const roomAgentsList = $('#room-agents-list');
  const allAgentsList = $('#all-agents-list');

  const mobileTabs = $('#mobile-tabs');

  // ── Harness icons ────────────────────────────────────────────────

  var harnessIconMap = {
    'claude-code': '/icons/claude-code.svg',
    'codex':       '/icons/codex.svg',
    'cursor':      '/icons/cursor.svg',
    'cline':       '/icons/cline.svg',
    'pi':          '/icons/pi.svg',
  };

  function harnessIconSrc(harness) {
    return harnessIconMap[harness] || '/icons/unknown.svg';
  }

  function agentIconSrc(agent) {
    if (!agent) return '/icons/unknown.svg';
    if (agent.kind === 'human') return '/icons/human.svg';
    return harnessIconSrc(agent.harness);
  }

  // ── Utility ──────────────────────────────────────────────────────

  function esc(str) {
    const div = document.createElement('div');
    div.textContent = str || '';
    return div.innerHTML;
  }

  function relativeTime(isoString) {
    if (!isoString) return 'never';
    const now = Date.now();
    const then = new Date(isoString).getTime();
    const diff = now - then;
    if (diff < 0) return 'just now';
    const seconds = Math.floor(diff / 1000);
    if (seconds < 5) return 'just now';
    if (seconds < 60) return seconds + 's ago';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + 'm ago';
    const hours = Math.floor(minutes / 60);
    if (hours < 24) return hours + 'h ago';
    const days = Math.floor(hours / 24);
    if (days < 30) return days + 'd ago';
    return new Date(isoString).toLocaleDateString();
  }

  function agentStatus(lastSeen) {
    if (!lastSeen) return 'offline';
    const diff = Date.now() - new Date(lastSeen).getTime();
    if (diff < 2 * 60 * 1000) return 'active';
    if (diff < 10 * 60 * 1000) return 'stale';
    return 'offline';
  }

  function isDM(roomID) {
    return roomID && roomID.startsWith('dm:');
  }

  function truncate(str, len) {
    if (!str) return '';
    return str.length > len ? str.substring(0, len) + '...' : str;
  }

  function initials(name) {
    if (!name) return '?';
    const parts = name.split(/[-_.\s]+/);
    if (parts.length >= 2) return (parts[0][0] + parts[1][0]).toUpperCase();
    return name.substring(0, 2).toUpperCase();
  }

  // ── Connection status ────────────────────────────────────────────

  function setConnected(connected) {
    if (connected) {
      connectionStatus.className = 'status-indicator connected';
      statusText.textContent = 'Connected';
    } else {
      connectionStatus.className = 'status-indicator disconnected';
      statusText.textContent = 'Disconnected';
    }
  }

  // ── API calls (HTTP — used for mutations only) ──────────────────

  function api(method, path, body) {
    var opts = {
      method: method,
      headers: { 'Content-Type': 'application/json' },
    };
    if (body) opts.body = JSON.stringify(body);
    return fetch(path, opts).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error('HTTP ' + r.status + ': ' + t); });
      return r.json();
    });
  }

  // registerHuman tells the server this UI user is a human with the current
  // agentID as their name. Idempotent on the server — safe to call on every
  // page load and name change. Required before any room operation (the
  // server rejects join/send from unregistered agents).
  function registerHuman() {
    return api('POST', '/agents', {
      kind: 'human',
      name: agentID,
      project: 'web-ui',
      meta: { via: 'web-ui' },
    }).then(function () {
      registered = true;
    }).catch(function (err) {
      registered = false;
      console.error('register failed:', err);
      // 409: name is in use by an AI — let the user know so they can change it.
      if (String(err).indexOf('409') >= 0 || String(err).indexOf('in use') >= 0) {
        alert('The name "' + agentID + '" is already in use by another agent on the bus. Change your name in the top bar.');
      }
      throw err;
    });
  }

  // ensureRegistered registers the human if not already done for the current
  // agentID. Returns a promise that resolves when the user is safe to use
  // room/message endpoints.
  function ensureRegistered() {
    if (registered) return Promise.resolve();
    return registerHuman();
  }

  function fetchRoomMessages(roomID) {
    return api('GET', '/rooms/' + encodeURIComponent(roomID) + '/messages?limit=100').then(function (data) {
      messages[roomID] = data.messages || [];
      // Reverse so oldest first (API returns newest first)
      messages[roomID].reverse();
      // Store last message preview
      if (messages[roomID].length > 0) {
        var last = messages[roomID][messages[roomID].length - 1];
        lastMessagePreview[roomID] = last.from + ': ' + last.body;
      }
      renderMessages();
    }).catch(function (err) {
      console.error('Failed to fetch messages for room ' + roomID + ':', err);
    });
  }

  // fetchRoomPresence pulls the current per-member presence snapshot for a
  // room. Called when opening a room so the member list reflects live state
  // immediately — subsequent updates arrive via WS.
  function fetchRoomPresence(roomID) {
    return api('GET', '/rooms/' + encodeURIComponent(roomID)).then(function (data) {
      mergeRoomPresence(roomID, data.members_presence);
      renderRoomAgents();
      renderReadReceipts();
    }).catch(function (err) {
      console.error('Failed to fetch room presence:', err);
    });
  }

  // fetchMyRooms loads this agent's room-view list (with unread counts and
  // cursors) so the sidebar renders accurate badges immediately after login.
  function fetchMyRooms() {
    return api('GET', '/agents/' + encodeURIComponent(agentID) + '/rooms').then(function (data) {
      var list = data.rooms || [];
      unreadCounts = {};
      readCursors = {};
      list.forEach(function (r) {
        unreadCounts[r.id] = r.unread_count || 0;
        readCursors[r.id] = r.read_cursor || 0;
      });
      renderRooms();
    }).catch(function (err) {
      console.error('Failed to fetch agent rooms:', err);
    });
  }

  // markRead tells the server this agent has read up to the latest message
  // in the given room. Called when the user opens a room. Fire-and-forget —
  // the server will broadcast a read_update WS event to sync other clients.
  function markRead(roomID) {
    if (!roomID || !registered) return;
    return api('POST', '/agents/' + encodeURIComponent(agentID) + '/read', {
      room: roomID,
      message_id: 0, // 0 = current HEAD
    }).then(function (data) {
      unreadCounts[roomID] = 0;
      readCursors[roomID] = data.read_cursor || readCursors[roomID] || 0;
      renderRooms();
    }).catch(function (err) {
      console.error('Failed to mark-read:', err);
    });
  }

  function joinRoom(roomID) {
    return ensureRegistered().then(function () {
      return api('POST', '/rooms/' + encodeURIComponent(roomID) + '/join', { agent_id: agentID });
    }).then(function () {
      // Server broadcasts room_update via WS — no need to fetch rooms
      selectRoom(roomID);
    });
  }

  function leaveRoom(roomID) {
    return api('POST', '/rooms/' + encodeURIComponent(roomID) + '/leave', { agent_id: agentID })
      .then(function () {
        if (activeRoomID === roomID) {
          wsUnsubscribeRoom(roomID);
          activeRoomID = null;
          prevSubscribedRoom = null;
          delete messages[roomID];
          showNoRoom();
        }
        // Server broadcasts room_update via WS
      })
      .catch(function (err) {
        console.error('Failed to leave room:', err);
      });
  }

  function sendMessage(body) {
    if (!activeRoomID) return;
    return ensureRegistered().then(function () {
      return api('POST', '/rooms/' + encodeURIComponent(activeRoomID) + '/send', {
        from: agentID,
        body: body,
      });
    }).catch(function (err) {
      console.error('Failed to send message:', err);
    });
  }

  function clearAll() {
    return api('DELETE', '/all').then(function () {
      rooms = [];
      messages = {};
      agents = [];
      activeRoomID = null;
      prevSubscribedRoom = null;
      lastMessagePreview = {};
      renderRooms();
      showNoRoom();
      renderAllAgents();
      renderRoomAgents();
      // Server broadcasts room_update + agent_update via WS
    });
  }

  // ── WebSocket ───────────────────────────────────────────────────

  function connectWS() {
    if (ws) {
      ws.close();
      ws = null;
    }

    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + '/ws');

    ws.onopen = function () {
      wsReconnectAttempts = 0;
      setConnected(true);

      // Identify ourselves so the server can keep our agent alive
      wsSend({ type: 'hello', agent_id: agentID });

      // Re-subscribe to active room if we had one
      if (activeRoomID) {
        wsSend({ type: 'subscribe', rooms: [activeRoomID] });
        prevSubscribedRoom = activeRoomID;
      }
    };

    ws.onmessage = function (event) {
      try {
        var frame = JSON.parse(event.data);
        switch (frame.type) {
          case 'message':
            handleWSMessage(frame.data);
            break;
          case 'room_update':
            handleWSRoomUpdate(frame.data);
            break;
          case 'agent_update':
            handleWSAgentUpdate(frame.data);
            break;
          case 'read_update':
            handleWSReadUpdate(frame.data);
            break;
          case 'presence':
            handleWSPresence(frame.data);
            break;
        }
      } catch (e) {
        console.error('WS parse error:', e);
      }
    };

    ws.onclose = function () {
      setConnected(false);
      ws = null;
      scheduleWSReconnect();
    };

    ws.onerror = function () {
      // onclose will fire after this
    };
  }

  function scheduleWSReconnect() {
    if (wsReconnectTimer) return;
    wsReconnectAttempts++;
    var delay = Math.min(1000 * Math.pow(2, wsReconnectAttempts - 1), 30000);
    wsReconnectTimer = setTimeout(function () {
      wsReconnectTimer = null;
      connectWS();
    }, delay);
  }

  function wsSend(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function wsSubscribeRoom(roomID) {
    if (prevSubscribedRoom && prevSubscribedRoom !== roomID) {
      wsSend({ type: 'unsubscribe', rooms: [prevSubscribedRoom] });
    }
    wsSend({ type: 'subscribe', rooms: [roomID] });
    prevSubscribedRoom = roomID;
  }

  function wsUnsubscribeRoom(roomID) {
    wsSend({ type: 'unsubscribe', rooms: [roomID] });
    if (prevSubscribedRoom === roomID) {
      prevSubscribedRoom = null;
    }
  }

  // ── WS event handlers ──────────────────────────────────────────

  function handleWSMessage(msg) {
    var roomID = msg.room_id;
    if (!roomID) return;

    if (!messages[roomID]) messages[roomID] = [];
    // Deduplicate
    if (messages[roomID].some(function (m) { return m.id === msg.id; })) return;
    messages[roomID].push(msg);

    // Update preview
    lastMessagePreview[roomID] = msg.from + ': ' + msg.body;

    // If this is the active room, append it AND keep the cursor in sync so
    // the badge doesn't linger after the user is literally watching it.
    if (roomID === activeRoomID) {
      appendMessage(msg);
      renderReadReceipts();
      // Own messages already advance the cursor server-side; mark-read any
      // others we just saw.
      if (msg.from !== agentID) {
        markRead(roomID);
      }
    } else {
      // Someone else's message in a room we're not looking at → unread++.
      // Our own messages are pre-marked-read by the server, don't badge them.
      if (msg.from !== agentID) {
        unreadCounts[roomID] = (unreadCounts[roomID] || 0) + 1;
      }
    }

    // Update room list preview + unread badge
    renderRooms();
  }

  function handleWSReadUpdate(data) {
    if (!data || !data.room) return;
    // The server emits a `presence` event alongside every read_update now,
    // so `presence[roomID][agentID]` will already reflect the new cursor.
    // Still patch readCursors/unread for the current user — unread badges
    // are separate from read-receipt avatars.
    if (data.agent_id === agentID) {
      readCursors[data.room] = data.read_cursor || 0;
      var msgs = messages[data.room] || [];
      var count = 0;
      for (var i = 0; i < msgs.length; i++) {
        if (msgs[i].id > (readCursors[data.room] || 0) && msgs[i].from !== agentID) {
          count++;
        }
      }
      unreadCounts[data.room] = count;
      renderRooms();
    }
    if (data.room === activeRoomID) {
      renderReadReceipts();
    }
  }

  // handleWSPresence records a single agent's {cursor, waiting} for a room
  // (or for the synthetic '*' bucket = agent-wide wait across any room).
  // Re-renders the member list and, if the affected room is visible, its
  // read-receipt markers.
  function handleWSPresence(data) {
    if (!data || !data.agent) return;
    var roomKey = data.room || '*';
    if (!presence[roomKey]) presence[roomKey] = {};
    presence[roomKey][data.agent] = {
      cursor: data.cursor || 0,
      waiting: !!data.waiting,
    };
    renderRoomAgents();
    if (roomKey === activeRoomID || roomKey === '*') {
      renderReadReceipts();
    }
  }

  // mergeRoomPresence ingests the `members_presence` array returned by
  // GET /rooms/{id}. Overwrites local room-scoped presence so stale entries
  // for agents who left don't linger.
  function mergeRoomPresence(roomID, list) {
    if (!roomID) return;
    var bucket = {};
    (list || []).forEach(function (p) {
      bucket[p.agent] = { cursor: p.cursor || 0, waiting: !!p.waiting };
    });
    presence[roomID] = bucket;
  }

  // effectivePresence returns {cursor, waiting} for (roomID, agentID),
  // merging the room-scoped entry with the agent-wide '*' bucket. An
  // agent-wide wait counts as "waiting" for every room the agent is in.
  function effectivePresence(roomID, agentID) {
    var room = (presence[roomID] || {})[agentID] || {};
    var global = (presence['*'] || {})[agentID] || {};
    return {
      cursor: room.cursor || 0,
      waiting: !!(room.waiting || global.waiting),
    };
  }

  // roomHead returns the highest message ID we've seen locally for a room.
  // 0 if we have no messages. Used to decide green vs yellow vs grey.
  function roomHead(roomID) {
    var msgs = messages[roomID] || [];
    return msgs.length > 0 ? msgs[msgs.length - 1].id : 0;
  }

  function handleWSRoomUpdate(data) {
    rooms = data.rooms || [];
    renderRooms();
    updateRoomHeader();
    renderRoomAgents();
  }

  function handleWSAgentUpdate(data) {
    agents = data.agents || [];
    renderAllAgents();
    renderRoomAgents();
  }

  // ── Room selection ───────────────────────────────────────────────

  function selectRoom(roomID) {
    if (activeRoomID === roomID) return;
    activeRoomID = roomID;

    // Show room view
    noRoomView.classList.add('hidden');
    roomView.classList.remove('hidden');

    // Update header
    updateRoomHeader();

    // Clear and fetch messages via HTTP (one-time load)
    renderMessages();
    fetchRoomMessages(roomID).then(function () {
      // Pull presence snapshot after messages so read-receipt rendering
      // has the head message id available.
      return fetchRoomPresence(roomID);
    });

    // Mark the room read as soon as the user opens it.
    markRead(roomID);

    // Subscribe to room messages via WebSocket
    wsSubscribeRoom(roomID);

    // Update sidebar highlights
    renderRooms();
    renderRoomAgents();

    // On mobile, switch to chat tab
    setMobileTab('chat');
  }

  function showNoRoom() {
    activeRoomID = null;
    noRoomView.classList.remove('hidden');
    roomView.classList.add('hidden');
    roomAgentsList.innerHTML = '<div class="empty-state">Select a room to see members.</div>';
    renderRooms();
  }

  function updateRoomHeader() {
    if (!activeRoomID) return;
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room) return;

    var dm = isDM(room.id);
    roomIconEl.textContent = dm ? '@' : '#';
    roomIconEl.className = 'room-header-icon' + (dm ? ' dm' : '');
    if (dm) {
      var others = (room.members || []).filter(function (m) { return m !== agentID; });
      roomNameEl.textContent = others.length > 0 ? others[0] : room.id;
    } else {
      roomNameEl.textContent = room.id;
    }
    var members = room.members || [];
    roomMemberCount.textContent = members.length + ' member' + (members.length !== 1 ? 's' : '');

    // Render avatars (max 6)
    var shown = members.slice(0, 6);
    roomMemberAvatars.innerHTML = shown.map(function (m) {
      return '<span class="member-avatar" title="' + esc(m) + '">' + esc(initials(m)) + '</span>';
    }).join('');
    if (members.length > 6) {
      roomMemberAvatars.innerHTML += '<span class="member-avatar" title="' + (members.length - 6) + ' more">+' + (members.length - 6) + '</span>';
    }
  }

  // ── Render rooms ─────────────────────────────────────────────────

  function renderRooms() {
    var channelRooms = rooms.filter(function (r) { return !isDM(r.id); });
    var dmRooms = rooms.filter(function (r) { return isDM(r.id); });

    function sortList(list) {
      return list.slice().sort(function (a, b) {
        var ma = (a.members || []).length;
        var mb = (b.members || []).length;
        if (mb !== ma) return mb - ma;
        return a.id.localeCompare(b.id);
      });
    }

    function roomItemHTML(r) {
      var dm = isDM(r.id);
      var isActive = r.id === activeRoomID;
      var members = r.members || [];
      var preview = lastMessagePreview[r.id] || '';
      var unread = unreadCounts[r.id] || 0;
      var hasUnread = unread > 0 && !isActive;
      var displayName = dm
        ? (members.length > 0 ? members.join(' · ') : r.id)
        : r.id;
      return (
        '<div class="room-item' +
          (isActive ? ' active' : '') +
          (dm ? ' dm' : '') +
          (hasUnread ? ' has-unread' : '') +
          '" data-room-id="' + esc(r.id) + '">' +
          '<span class="room-item-icon">' + (dm ? '@' : '#') + '</span>' +
          '<div class="room-item-info">' +
            '<div class="room-item-top">' +
              '<span class="room-item-name">' + esc(displayName) + '</span>' +
              '<span class="room-item-count">' + members.length + '</span>' +
              (hasUnread ? '<span class="room-item-unread">' + unread + '</span>' : '') +
            '</div>' +
            (preview ? '<div class="room-item-preview">' + esc(truncate(preview, 40)) + '</div>' : '') +
          '</div>' +
        '</div>'
      );
    }

    function attachClickHandlers(container) {
      container.querySelectorAll('.room-item').forEach(function (el) {
        el.addEventListener('click', function () {
          var rid = el.getAttribute('data-room-id');
          var room = rooms.find(function (r) { return r.id === rid; });
          var isMember = room && room.members && room.members.indexOf(agentID) !== -1;
          if (isMember) {
            selectRoom(rid);
          } else {
            joinRoom(rid);
          }
        });
      });
    }

    if (channelRooms.length === 0) {
      roomListEl.innerHTML = '<div class="empty-state">No rooms yet. Join or create one above.</div>';
    } else {
      roomListEl.innerHTML = sortList(channelRooms).map(roomItemHTML).join('');
      attachClickHandlers(roomListEl);
    }

    if (dmRooms.length === 0) {
      dmListEl.innerHTML = '<div class="empty-state">No direct messages yet.</div>';
    } else {
      dmListEl.innerHTML = sortList(dmRooms).map(roomItemHTML).join('');
      attachClickHandlers(dmListEl);
    }
  }

  // ── Render messages ──────────────────────────────────────────────

  function renderMessages() {
    if (!activeRoomID) return;
    var msgs = messages[activeRoomID] || [];

    if (msgs.length === 0) {
      messageListEl.innerHTML = '<div class="empty-state">No messages in this room yet.</div>';
      return;
    }

    messageListEl.innerHTML = msgs.map(function (m) {
      return chatMessageHTML(m);
    }).join('');

    renderReadReceipts();
    scrollToBottom();
  }

  function chatMessageHTML(m) {
    var isSelf = m.from === agentID;
    var fromAgent = agents.find(function (a) { return a.id === m.from; });
    var msgIconSrc = agentIconSrc(fromAgent);
    var msgIconTitle = fromAgent
      ? (fromAgent.kind === 'human' ? 'human' : esc((fromAgent.model || 'unknown') + ' · ' + (fromAgent.harness || 'unknown')))
      : 'unknown';
    var msgIconAlt = fromAgent
      ? (fromAgent.kind === 'human' ? 'human' : esc(fromAgent.harness || 'unknown'))
      : 'unknown';
    return (
      '<div class="chat-msg' + (isSelf ? ' self' : '') + '" data-id="' + esc(m.id) + '">' +
        '<div class="chat-msg-header">' +
          '<span class="chat-msg-from">' +
            '<img src="' + msgIconSrc + '" class="harness-icon chat-msg-icon" alt="' + msgIconAlt + '" title="' + msgIconTitle + '" width="14" height="14">' +
            '<span class="chat-msg-from-name">' + esc(m.from) + '</span>' +
          '</span>' +
          '<span class="chat-msg-time" title="' + esc(m.created_at) + '">' + relativeTime(m.created_at) + '</span>' +
        '</div>' +
        '<div class="chat-msg-bubble">' +
          '<div class="chat-msg-body">' + esc(m.body) + '</div>' +
        '</div>' +
      '</div>'
    );
  }

  function appendMessage(msg) {
    if (!activeRoomID || msg.room_id !== activeRoomID) return;

    // Remove empty state if present
    var empty = messageListEl.querySelector('.empty-state');
    if (empty) empty.remove();

    var temp = document.createElement('div');
    temp.innerHTML = chatMessageHTML(msg);
    var el = temp.firstChild;
    el.classList.add('new-message');
    messageListEl.appendChild(el);

    scrollToBottom();
  }

  function scrollToBottom() {
    requestAnimationFrame(function () {
      messageListEl.scrollTop = messageListEl.scrollHeight;
    });
  }

  // ── Render agents ────────────────────────────────────────────────

  function renderAllAgents() {
    if (agents.length === 0) {
      allAgentsList.innerHTML = '<div class="empty-state">No agents registered.</div>';
      return;
    }

    var sorted = agents.slice().sort(function (a, b) {
      var sa = agentStatus(a.last_seen);
      var sb = agentStatus(b.last_seen);
      var order = { active: 0, stale: 1, offline: 2 };
      if (order[sa] !== order[sb]) return order[sa] - order[sb];
      return new Date(b.last_seen || 0) - new Date(a.last_seen || 0);
    });

    allAgentsList.innerHTML = sorted.map(function (a) {
      return agentCardHTML(a);
    }).join('');
  }

  function renderRoomAgents() {
    if (!activeRoomID) {
      roomAgentsList.innerHTML = '<div class="empty-state">Select a room to see members.</div>';
      return;
    }

    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    if (!room || !room.members || room.members.length === 0) {
      roomAgentsList.innerHTML = '<div class="empty-state">No members in this room.</div>';
      return;
    }

    roomAgentsList.innerHTML = room.members.map(function (memberID) {
      var agent = agents.find(function (a) { return a.id === memberID; });
      if (agent) {
        return agentCardHTML(agent);
      }
      return (
        '<div class="agent-card">' +
          '<div class="agent-id">' +
            '<span class="agent-online-dot offline"></span>' +
            esc(memberID) +
          '</div>' +
        '</div>'
      );
    }).join('');
  }

  function agentCardHTML(a) {
    var status = agentStatus(a.last_seen);
    var meta = a.meta || {};
    var metaKeys = Object.keys(meta);
    var metaTags = metaKeys.map(function (k) {
      return '<span class="meta-tag" title="' + esc(k) + '"><span class="meta-key">' + esc(k) + '</span> ' + esc(meta[k]) + '</span>';
    }).join('');
    // Presence dot: only meaningful inside a specific room, so only render
    // when we're viewing a room AND this agent is a member.
    var presenceTag = '';
    if (activeRoomID) {
      var room = rooms.find(function (r) { return r.id === activeRoomID; });
      var isMember = room && room.members && room.members.indexOf(a.id) !== -1;
      if (isMember) {
        var p = effectivePresence(activeRoomID, a.id);
        var head = roomHead(activeRoomID);
        var cls, title;
        if (p.waiting) {
          cls = 'waiting';
          title = 'Listening live (bus_wait open)';
        } else if (head > 0 && p.cursor < head) {
          cls = 'behind';
          title = 'Behind head (not currently waiting) — cursor at #' + p.cursor + ' of #' + head;
        } else {
          cls = 'idle';
          title = 'Caught up, not waiting';
        }
        presenceTag = '<span class="agent-presence-dot ' + cls + '" title="' + esc(title) + '"></span>';
      }
    }
    var iconSrc = agentIconSrc(a);
    var iconTitle = a.kind === 'human' ? 'human' : esc((a.model || 'unknown') + ' · ' + (a.harness || 'unknown'));
    var iconAlt = a.kind === 'human' ? 'human' : esc(a.harness || 'unknown');
    var iconTag = '<img src="' + iconSrc + '" class="harness-icon" alt="' + iconAlt + '" title="' + iconTitle + '" width="14" height="14">';
    return (
      '<div class="agent-card">' +
        '<div class="agent-id">' +
          '<span class="agent-online-dot ' + status + '"></span>' +
          presenceTag +
          iconTag +
          esc(a.id) +
        '</div>' +
        (metaTags ? '<div class="agent-meta">' + metaTags + '</div>' : '') +
        '<div class="agent-lastseen">Last seen: ' + relativeTime(a.last_seen) + '</div>' +
      '</div>'
    );
  }

  // renderReadReceipts appends a small avatar strip to each chat message
  // in the active room, one per agent whose effective cursor >= msg.id
  // (excluding the message author — sender is implicitly read).
  function renderReadReceipts() {
    if (!activeRoomID) return;
    var msgs = messages[activeRoomID] || [];
    var room = rooms.find(function (r) { return r.id === activeRoomID; });
    var members = (room && room.members) || [];
    msgs.forEach(function (m) {
      var el = messageListEl.querySelector('[data-id="' + esc(m.id) + '"]');
      if (!el) return;
      var seenBy = members.filter(function (memberID) {
        if (memberID === m.from) return false; // sender already saw it
        var p = effectivePresence(activeRoomID, memberID);
        return p.cursor >= m.id;
      });
      var strip = el.querySelector('.chat-msg-receipts');
      if (seenBy.length === 0) {
        if (strip) strip.remove();
        return;
      }
      var html = seenBy.map(function (memberID) {
        return '<span class="receipt-avatar" title="Seen by ' + esc(memberID) + '">' + esc(initials(memberID)) + '</span>';
      }).join('');
      if (!strip) {
        strip = document.createElement('div');
        strip.className = 'chat-msg-receipts';
        el.appendChild(strip);
      }
      strip.innerHTML = html;
    });
  }

  // ── Mobile tab handling ──────────────────────────────────────────

  function setMobileTab(tab) {
    document.body.className = 'tab-' + tab;
    mobileTabs.querySelectorAll('.mobile-tab').forEach(function (el) {
      el.classList.toggle('active', el.getAttribute('data-tab') === tab);
    });
  }

  // ── Event handlers ───────────────────────────────────────────────

  // Agent ID — persist to localStorage. On change, re-register under the new
  // name so the user can use the bus without manual re-auth.
  agentIDInput.value = agentID;
  agentIDInput.addEventListener('change', function () {
    var newID = agentIDInput.value.trim();
    if (newID && newID !== agentID) {
      agentID = newID;
      localStorage.setItem('aimebu_agent_id', newID);
      registered = false;
      unreadCounts = {};
      readCursors = {};
      registerHuman().then(function () {
        return fetchMyRooms();
      }).catch(function () {});
    }
  });

  agentIDInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      agentIDInput.blur();
    }
  });

  // Settings dropdown
  settingsBtn.addEventListener('click', function (e) {
    e.stopPropagation();
    settingsDropdown.classList.toggle('hidden');
  });

  document.addEventListener('click', function () {
    settingsDropdown.classList.add('hidden');
  });

  settingsDropdown.addEventListener('click', function (e) {
    e.stopPropagation();
  });

  // Clear all
  clearAllBtn.addEventListener('click', function () {
    if (!confirm('Clear all rooms, messages, and agents? This cannot be undone.')) return;
    clearAll().catch(function (err) {
      alert('Error: ' + err.message);
    });
  });

  // Join room
  function handleJoinRoom() {
    var roomID = joinRoomInput.value.trim();
    if (!roomID) return;
    joinRoomBtn.disabled = true;
    joinRoom(roomID)
      .then(function () {
        joinRoomInput.value = '';
      })
      .catch(function (err) {
        console.error('Failed to join room:', err);
        alert('Failed to join room: ' + err.message);
      })
      .finally(function () {
        joinRoomBtn.disabled = false;
      });
  }

  joinRoomBtn.addEventListener('click', handleJoinRoom);
  joinRoomInput.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') {
      e.preventDefault();
      handleJoinRoom();
    }
  });

  // Leave room
  leaveRoomBtn.addEventListener('click', function () {
    if (!activeRoomID) return;
    leaveRoom(activeRoomID);
  });

  // Send message
  sendForm.addEventListener('submit', function (e) {
    e.preventDefault();
    var body = msgBodyInput.value.trim();
    if (!body) return;
    sendMessage(body);
    msgBodyInput.value = '';
    msgBodyInput.focus();
  });

  // Mobile tabs
  mobileTabs.querySelectorAll('.mobile-tab').forEach(function (tab) {
    tab.addEventListener('click', function () {
      setMobileTab(tab.getAttribute('data-tab'));
    });
  });

  // ── Periodic refresh (timestamps only) ──────────────────────────

  // Update relative timestamps every 30 seconds (purely cosmetic)
  setInterval(function () {
    renderMessages();
    renderRooms();
    renderAllAgents();
    renderRoomAgents();
  }, 30000);

  // ── Init ─────────────────────────────────────────────────────────

  setMobileTab('rooms');

  // If no persisted name yet, try to prefill from the server's $AIMEBU_NAME.
  var prefillPromise;
  if (!agentIDFromStorage) {
    prefillPromise = fetch('/default-name').then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (data) {
      if (data && data.name) {
        agentID = data.name;
        agentIDInput.value = agentID;
      }
    }).catch(function () {});
  } else {
    prefillPromise = Promise.resolve();
  }

  // Register as a human on the bus before opening the websocket. If
  // registration fails (e.g. name clash with an AI) we still connect —
  // subsequent operations will retry via ensureRegistered and surface the
  // error to the user.
  prefillPromise.then(function () {
    return registerHuman().catch(function () {});
  }).then(function () {
    return fetchMyRooms().catch(function () {});
  }).finally(function () {
    connectWS();
  });
})();
