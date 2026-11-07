// The Spawn dialog's "Continue a Claude Code session" picker, over a fixture Claude home: the sessions of the project's folder with their flags, finding one by id, picking one and what the dialog then says.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn, spawnSync } = require('node:child_process');
const { start, isolatedEnv, binary } = require('./ui-smoke-lib.cjs');

async function main() {
  const ctx = await start();
  const { tempRoot, tmp, gitRoot, testHome, seedBinary, APP_READY_TIMEOUT, appReadyFolded, appReady, freePort, waitDump, waitRoomRow, installFixtureChat, child, browser, supervisor, createBrowserContext, a, b, first, second, origin, port, domClick, reachRow, projectMenuAct, iconOnlyReport, disableRandomUUID, answerDialogs, login, setChecked, settingsNav, openSettings, closeRoomInfo, openProjectSettings, settingsTab, welcomeState, settled, iconState, assertIcon, assertFinished, skipHarnessOffer } = ctx;
  try {
    await login(first);
    // The project the step works in: the scratch git repository, as the smoke creates it through the page.
    await first.evaluate(async folder => {
      const { csrf_token } = await (await fetch('/api/me')).json();
      const response = await fetch('/api/projects', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': csrf_token }, body: JSON.stringify({ name: 'UI smoke', folder }) });
      if (!response.ok) throw new Error(`could not create the project: ${response.status} ${await response.text()}`);
    }, gitRoot);
    await first.goto(origin);
    await appReady(first);
    await first.waitForFunction(() => document.querySelector('#room-title')?.textContent === 'UI smoke', { timeout: 60000 });
    const actualProjectID = await first.evaluate(async () => (await (await fetch('/api/projects')).json()).find(p => p.name === 'UI smoke').id);
    // Continue a Claude Code session: the Spawn dialog's session button opens a picker of what the machine's supervisor finds in its Claude
    // home (a fixture one here, never the real ~/.claude), by folder or by id; a pick reopens the Spawn dialog for that session.
    {
      const claudeHome = path.join(testHome, '.claude');
      const folderName = folder => folder.replace(/[^a-zA-Z0-9]/g, '-');
      const elsewhere = path.join(tmp, 'elsewhere'); fs.mkdirSync(elsewhere, { recursive: true });
      const ccSession = (cwd, id, { title, entrypoint = 'cli', branch = 'main', model = '' }) => {
        const dir = path.join(claudeHome, 'projects', folderName(cwd)); fs.mkdirSync(dir, { recursive: true });
        const lines = [{ type: 'user', cwd, sessionId: id, gitBranch: branch, version: '2.1.309', entrypoint, message: { role: 'user', content: 'start' } },
          { type: 'assistant', cwd, sessionId: id, message: { role: 'assistant', content: 'ok', ...(model ? { model } : {}) } }, { type: 'ai-title', aiTitle: title, sessionId: id }];
        fs.writeFileSync(path.join(dir, `${id}.jsonl`), lines.map(line => JSON.stringify(line)).join('\n') + '\n');
      };
      const [idOne, idActive, idAgent, idFar] = ['11111111-aaaa-4aaa-8aaa-000000000001', '11111111-aaaa-4aaa-8aaa-000000000002', '11111111-aaaa-4aaa-8aaa-000000000003', '11111111-aaaa-4aaa-8aaa-000000000004'];
      ccSession(gitRoot, idOne, { title: 'Fix the login bug', model: 'claude-opus-4-5-20251101' });
      ccSession(gitRoot, idActive, { title: 'Open in a terminal' });
      ccSession(gitRoot, idAgent, { title: 'Made by an agent', entrypoint: 'sdk-cli' });
      ccSession(elsewhere, idFar, { title: 'Worked elsewhere' });
      fs.mkdirSync(path.join(claudeHome, 'sessions'), { recursive: true });
      const isPidAlive = pid => { try { process.kill(pid, 0); return true; } catch (error) { return error.code === 'EPERM'; } };
      // This process is the live pid; the gone one is above any kernel's pid limit, so it can never be assigned to a process (a stale entry, ignored).
      const gonePid = 2 ** 31 - 2;
      assert.ok(isPidAlive(process.pid) && !isPidAlive(gonePid), 'the fixture has one pid that is alive and one that is gone');
      for (const [pid, sessionId] of [[process.pid, idActive], [gonePid, idOne]]) fs.writeFileSync(path.join(claudeHome, 'sessions', `${pid}.json`), JSON.stringify({ pid, sessionId, cwd: gitRoot, kind: 'interactive', status: 'idle' }));
      // Whether a native Claude is on offer comes from the machine's tool detection, which can still be running when this step starts. The
      // dialog says a session "is not available natively" until the catalog lists one it can continue on (spawn-model.js resumeToolChoice: native, or a container
      // that shares the host's Claude home), so wait for the catalog the dialog will read to do so.
      await first.waitForFunction(async () => (await (await fetch('/api/spawn-catalog')).json()).tools?.some(tool => tool.tool === 'claude' && tool.available && !(tool.containerized && !tool.shares_claude_home)), { polling: 500, timeout: 30000 }).catch(() => {});
      await domClick(first, '#spawn-open');
      await first.waitForFunction(() => document.querySelector('#spawn-dialog').open && document.querySelector('#spawn-sessions-menu') && !document.querySelector('#spawn-sessions').hidden);
      // The entry is the item of the ⋯ menu in the title row: a labelled button, a menu that is closed at rest, and an item that says what it does.
      assert.deepEqual(await first.evaluate(() => { const menu = document.querySelector('#spawn-sessions-menu'), item = document.querySelector('#spawn-sessions'); return [menu.title, menu.getAttribute('aria-label'), menu.getAttribute('aria-expanded'), item.textContent.trim(), !!item.offsetParent]; }), ['More', 'More', 'false', 'Continue a Claude Code session…', false], 'the session entry is a labelled ⋯ button with a closed menu, and its item says what it does');
      assert.deepEqual(await first.evaluate(() => { const item = document.querySelector('#spawn-sessions'), menu = document.querySelector('#spawn-sessions-menu'), model = document.querySelector('#spawn-form [name=machine]'); return { enabled: !item.disabled, note: document.querySelector('#spawn-sessions-note').hidden, text: !!item.textContent.trim(), inTitle: !!menu.closest('.spawn-title-row') && !!model }; }), { enabled: true, note: true, text: true, inTitle: true }, 'the session entry is usable with a launcher that can continue a session, and says nothing then');
      // The menu opens on its item; the item opens the picker.
      const continueSession = async () => { await domClick(first, '#spawn-sessions-menu'); await first.waitForSelector('#spawn-sessions', { visible: true }); await domClick(first, '#spawn-sessions'); };
      await continueSession();
      await first.waitForFunction(() => document.querySelector('#cc-sessions-dialog').open && document.querySelectorAll('#cc-sessions-list .cc-session').length === 3 && !document.querySelector('#spawn-dialog').open);
      assert.deepEqual(await first.$$eval('#cc-sessions-list .cc-session', rows => rows.map(row => [row.dataset.sessionId, row.querySelector('.cc-session-title').textContent, [...row.querySelectorAll('.cc-session-flag')].map(flag => flag.textContent)]).sort()),
        [[idOne, 'Fix the login bug', []], [idActive, 'Open in a terminal', ['open in a terminal']], [idAgent, 'Made by an agent', ['started by an agent']]], 'the picker lists the folder\'s sessions with their flags');
      assert.ok(await first.$$eval('#cc-sessions-list .cc-session-pick', buttons => buttons.every(button => button.title === 'Continue this session or work on a copy' && button.getAttribute('aria-label') === 'Continue this session or work on a copy' && !button.textContent.trim())), 'each session has an icon button with a tooltip');
      assert.equal(await first.$eval('#cc-sessions-status', el => el.textContent), '3 sessions');
      // The paste-an-id field is the alternative to the list, so it sits below it; the flags are one column with their right edges on one line.
      assert.deepEqual(await first.evaluate(() => { const list = document.querySelector('#cc-sessions-list').getBoundingClientRect(), find = document.querySelector('#cc-sessions-find').getBoundingClientRect(), edges = [...document.querySelectorAll('#cc-sessions-list .cc-session-flags')].filter(box => box.children.length).map(box => Math.round(box.getBoundingClientRect().right));
        return { below: find.top >= list.bottom, sameEdge: new Set(edges).size === 1, flagged: edges.length }; }), { below: true, sameEdge: true, flagged: 2 }, 'the id field is under the list and the badges line up');
      // By id it is found wherever it worked, and says it is outside the project folder; an unknown id says so.
      await first.$eval('#cc-session-id', (el, id) => { el.value = id; }, idFar);
      await first.evaluate(() => document.querySelector('#cc-sessions-find').requestSubmit());
      await first.waitForFunction(() => document.querySelectorAll('#cc-sessions-list .cc-session').length === 1 && document.querySelector('#cc-sessions-list .cc-session-cwd'));
      assert.deepEqual(await first.$eval('#cc-sessions-list .cc-session', row => [row.dataset.sessionId, [...row.querySelectorAll('.cc-session-flag')].map(flag => flag.textContent), row.querySelector('.cc-session-cwd').textContent]), [idFar, ['outside the project folder'], elsewhere]);
      await first.$eval('#cc-session-id', el => { el.value = '22222222-bbbb-4bbb-8bbb-999999999999'; });
      await first.evaluate(() => document.querySelector('#cc-sessions-find').requestSubmit());
      await first.waitForFunction(() => document.querySelector('#cc-sessions-error').textContent.includes('no Claude Code session with that id'));
      // Picking one reopens the Spawn dialog for it: a room of its own by default, the name kept, nothing to confirm while nothing has it open.
      await domClick(first, '#cc-sessions-close');
      await domClick(first, '#spawn-open');
      await first.waitForFunction(() => document.querySelector('#spawn-dialog').open);
      await continueSession();
      await first.waitForFunction(() => document.querySelectorAll('#cc-sessions-list .cc-session').length === 3);
      await domClick(first, `#cc-sessions-list .cc-session[data-session-id="${idOne}"] .cc-session-pick`);
      await first.waitForSelector('#spawn-dialog[open] #spawn-resume:not([hidden])');
      // The model the session ran on is what the dialog preselects (by family), and a different one is warned about. (A scratch server without a
      // native Claude harness has no model to select: then the dialog says the session cannot be continued here, and there is nothing to compare.)
      await first.waitForFunction(() => document.querySelector('#spawn-form [name=model]').options.length > 0 || document.querySelector('#spawn-resume-notes').textContent.includes('not available natively'));
      const modelNote = await first.evaluate(() => ({ options: document.querySelector('#spawn-form [name=model]').options.length, selected: document.querySelector('#spawn-form [name=model]').selectedOptions[0]?.textContent || '', notes: document.querySelector('#spawn-resume-notes').textContent }));
      if (modelNote.options) {
        await first.waitForFunction(() => /opus/i.test(document.querySelector('#spawn-form [name=model]').selectedOptions[0]?.textContent || '') || document.querySelector('#spawn-resume-notes').textContent.includes('This session ran on'));
        const settled = await first.evaluate(() => ({ selected: document.querySelector('#spawn-form [name=model]').selectedOptions[0]?.textContent || '', notes: document.querySelector('#spawn-resume-notes').textContent }));
        assert.ok(/opus/i.test(settled.selected) ? !settled.notes.includes('This session ran on') : settled.notes.includes('This session ran on claude-opus-4-5-20251101; the agent will continue it on'), `the session's model is preselected or warned about: ${JSON.stringify(settled)}`);
      }
      // The Continue/Fork radios sit inline with their words, not stacked above them as full-width blocks.
      assert.deepEqual(await first.evaluate(() => [...document.querySelectorAll('#spawn-resume-mode input')].filter(input => input.offsetParent).map(input => { const label = input.closest('label'), box = input.getBoundingClientRect(), row = label.getBoundingClientRect(); return [getComputedStyle(label).display, box.top >= row.top - 1 && box.bottom <= row.bottom + 1, box.width < 30]; })), [['flex', true, true], ['flex', true, true]], 'the Continue and Fork radios are inline with their labels');
      assert.equal(await first.evaluate(() => { const input = document.querySelector('#spawn-resume-confirm'); return `${getComputedStyle(input).display}/${getComputedStyle(input).width !== '100%'}`; }), 'inline-block/true', 'the confirmation box is not a full-width block');
      assert.deepEqual(await first.evaluate(() => {
        const heights = []; for (const row of document.querySelectorAll('.machine-actions, .dialog-actions')) { const icon = row.querySelector(':scope > .btn-icon'), text = row.querySelector(':scope > .btn'); if (icon && text && icon.offsetParent) heights.push(Math.round(icon.getBoundingClientRect().height) === Math.round(text.getBoundingClientRect().height)); } return heights.every(Boolean);
      }), true, 'an icon button beside a text button is as tall as it');
      assert.deepEqual(await first.evaluate(() => ({
        title: document.querySelector('#spawn-form h2').textContent, text: document.querySelector('#spawn-resume-text').textContent, confirm: document.querySelector('#spawn-resume-confirm-label').hidden,
        name: document.querySelector('.spawn-name').hidden, startIn: !document.querySelector('#spawn-start-in').hidden, room: document.querySelector('#spawn-start-room').selectedOptions[0]?.textContent,
        sessions: !document.querySelector('#spawn-sessions-menu'), worktree: document.querySelector('.spawn-worktree-option').hidden,
        mode: document.querySelector('#spawn-resume-mode').hidden ? 'hidden' : document.querySelector('#spawn-form [name=resume_mode]:checked').value,
      })), { title: 'Continue a Claude Code session', text: 'This continues the session. It carries on from where it stopped, in the same transcript.',
        confirm: true, name: false, startIn: true, room: 'A new room for this session', sessions: true, worktree: true, mode: 'continue' }, 'the Spawn dialog opens for the picked session, continuing by default');
      // The session is a neutral card: its short id, its folder and when it was last used, in a compact box; no title from the first prompt.
      assert.deepEqual(await first.evaluate(() => { const box = document.querySelector('#spawn-resume'), card = document.querySelector('#spawn-resume-card'), modes = [...document.querySelectorAll('#spawn-resume-mode label')].map(label => label.getBoundingClientRect().top); return { rows: [...card.querySelectorAll('dt')].map(term => [term.textContent, term.nextElementSibling.textContent.length > 0]), id: card.querySelector('dd').textContent, oneRow: Math.abs(modes[0] - modes[1]) < 2, nameRow: [Math.round(document.querySelector('#spawn-name').getBoundingClientRect().height) === Math.round(document.querySelector('#spawn-name-dice').getBoundingClientRect().height), getComputedStyle(document.querySelector('.spawn-name > label')).fontSize === getComputedStyle(document.querySelector('#spawn-dialog > form > label')).fontSize], titled: box.textContent.includes('Fix the login bug') }; }),
        { rows: [['Session', true], ['Folder', true], ['Last active', true], ['Ran on', true]], id: '11111111', oneRow: true, nameRow: [true, true], titled: false }, 'the picked session is shown as a neutral card, the Continue and Fork choice is one row, and the Name row matches the other fields');
      await domClick(first, '#spawn-cancel');
      await first.waitForFunction(() => document.querySelector('#spawn-resume').hidden);
      // A session a live process has open asks for a confirmation before it can be continued.
      await domClick(first, '#spawn-open');
      await first.waitForFunction(() => document.querySelector('#spawn-dialog').open);
      await continueSession();
      await first.waitForFunction(() => document.querySelectorAll('#cc-sessions-list .cc-session').length === 3);
      await domClick(first, `#cc-sessions-list .cc-session[data-session-id="${idActive}"] .cc-session-pick`);
      await first.waitForSelector('#spawn-dialog[open] #spawn-resume:not([hidden])');
      const activeSession = await first.evaluate(() => ({ confirm: document.querySelector('#spawn-resume-confirm-label').hidden, label: document.querySelector('#spawn-resume-confirm-label').textContent.trim(), notes: document.querySelector('#spawn-resume-notes').textContent }));
      assert.deepEqual({ confirm: activeSession.confirm, label: activeSession.label }, { confirm: false, label: 'I closed this session in my terminal' });
      const activeWarning = 'A Claude process has this session open. Close it first: two processes on one session corrupt it.';
      assert.ok(activeSession.notes === activeWarning || activeSession.notes === `${activeWarning} claude is not available natively on the chosen machine, so the session cannot be continued here.`, `active-session notes: ${activeSession.notes}`);
      // Working on a copy needs no such confirmation (the original is not touched) and says what it does; going back restores the warning.
      await first.evaluate(() => { const radio = document.querySelector('#spawn-form [name=resume_mode][value=fork]'); radio.checked = true; radio.dispatchEvent(new Event('change', { bubbles: true })); });
      assert.deepEqual(await first.evaluate(() => ({ title: document.querySelector('#spawn-form h2').textContent, confirm: document.querySelector('#spawn-resume-confirm-label').hidden,
        text: document.querySelector('#spawn-resume-text').textContent.slice(0, 60), notes: document.querySelector('#spawn-resume-notes').textContent.includes('Close it first') })),
        { title: 'Work on a copy of a Claude Code session', confirm: true, text: 'This works on a copy of the session. The agent starts from e', notes: false });
      await first.evaluate(() => { const radio = document.querySelector('#spawn-form [name=resume_mode][value=continue]'); radio.checked = true; radio.dispatchEvent(new Event('change', { bubbles: true })); });
      assert.equal(await first.$eval('#spawn-resume-confirm-label', el => el.hidden), false, 'continuing asks again for the confirmation');
      await domClick(first, '#spawn-cancel');
      await first.waitForFunction(() => document.querySelector('#spawn-resume').hidden);
      assert.equal(await first.$eval('#spawn-form h2', el => el.textContent), 'Spawn', 'closing the dialog restores the ordinary one');
      // Bulk import (the home room's settings → Project): the dialog is driven here with a scripted source, so no agent is really started. Roles are preset
      // (the first session of a folder works, the rest review), what is ticked is started in order as copies in rooms of their own, and a
      // spawn limit stops the rest and says so.
      const imported = await first.evaluate(async projectID => {
        const { ClaudeSessionImport } = await import('/js/pages/claude-import/page.js');
        const calls = [];
        const sessions = [
          { id: 'aaaaaaaa-0000-4000-8000-000000000001', title: 'First of /p', cwd: '/p', modified_at: '2026-10-24T10:00:00Z', size: 100 },
          { id: 'aaaaaaaa-0000-4000-8000-000000000002', title: 'Second of /p', cwd: '/p', modified_at: '2026-10-24T09:00:00Z', size: 100, agent: { slug: 'olive' } },
          { id: 'aaaaaaaa-0000-4000-8000-000000000003', title: 'Outside', cwd: '/q', modified_at: '2026-10-24T08:00:00Z', size: 100, outside_folder: true },
        ];
        const source = {
          roles: async () => [{ role_key: 'worker', label: 'Worker' }, { role_key: 'reviewer', label: 'Reviewer' }, { role_key: 'lead', label: 'Lead', cap_is_lead: true }],
          catalog: async () => ({ tools: [{ tool: 'claude', variant: 'docker', launcher_id: 'l_docker', containerized: true, available: true, label: 'Claude Code (Docker)', models: [{ id: 'haiku' }] },
            { tool: 'claude', variant: 'native', launcher_id: 'l_native', containerized: false, available: true, label: 'Claude Code', models: [{ id: 'opus', label: 'Opus' }, { id: 'haiku', label: 'Haiku', default: true }] }] }),
          claudeSessions: async (project, options) => { calls.push(['list', project, options.launcherID]); return { sessions, unreadable: 2, truncated: false }; },
          spawn: async (room, params) => {
            calls.push(['spawn', room, params.role, params.resume_session.slice(-1), params.resume_mode, params.new_room, params.launcher_id, params.model]);
            if (params.resume_session.endsWith('3')) throw Object.assign(new Error('agent or spawn rate limit reached'), { code: 'spawn_limit' });
            return { slug: 'copy-' + params.resume_session.slice(-1) };
          },
        };
        const dialog = new ClaudeSessionImport(source);
        await dialog.open({ projectID, homeRoomID: 'home_room' });
        const waitFor = async test => { for (let i = 0; i < 100 && !test(); i++) await new Promise(resolve => setTimeout(resolve, 20)); };
        await waitFor(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 3);
        const shown = {
          harnesses: [...document.querySelectorAll('#cc-import-harness option')].map(option => option.textContent),
          models: [...document.querySelectorAll('#cc-import-model option')].map(option => [option.value, option.selected]),
          roles: [...document.querySelectorAll('#cc-import-list select')].map(select => select.value),
          roleOptions: [...document.querySelector('#cc-import-list select').options].map(option => option.value),
          status: document.querySelector('#cc-import-status').textContent,
          runDisabled: document.querySelector('#cc-import-run').disabled,
          flags: [...document.querySelectorAll('#cc-import-list .cc-import-row')].map(row => [...row.querySelectorAll('.cc-session-flag')].map(flag => flag.textContent)),
        };
        document.querySelector('#cc-import-all').click();
        await new Promise(resolve => setTimeout(resolve, 0)); // the page draws what the click changed
        shown.afterAll = { ticked: document.querySelectorAll('#cc-import-list input:checked').length, runDisabled: document.querySelector('#cc-import-run').disabled };
        document.querySelector('#cc-import-list select').value = 'reviewer';
        document.querySelector('#cc-import-run').click();
        await waitFor(() => /started/.test(document.querySelector('#cc-import-status').textContent));
        shown.states = [...document.querySelectorAll('#cc-import-list .cc-import-state')].map(state => state.textContent);
        shown.final = document.querySelector('#cc-import-status').textContent;
        shown.afterRun = { ticked: document.querySelectorAll('#cc-import-list input:checked').length, locked: [...document.querySelectorAll('#cc-import-list input')].map(box => box.disabled) };
        shown.calls = calls;
        document.querySelector('#cc-import-close').click();
        return shown;
      }, actualProjectID);
      assert.deepEqual({ harnesses: imported.harnesses, models: imported.models, roles: imported.roles, roleOptions: imported.roleOptions, status: imported.status, runDisabled: imported.runDisabled, flags: imported.flags }, {
        harnesses: ['Claude Code'], models: [['opus', false], ['haiku', true]], roles: ['worker', 'reviewer', 'worker'], roleOptions: ['worker', 'reviewer'],
        status: '3 sessions (2 sessions could not be read)', runDisabled: true, flags: [[], ['continued by olive'], ['outside the project folder', 'needs approval']] }, 'the import dialog lists the sessions with preset roles and only a native harness');
      assert.deepEqual(imported.afterAll, { ticked: 3, runDisabled: false });
      assert.deepEqual(imported.calls, [['list', actualProjectID, 'l_native'],
        ['spawn', 'home_room', 'reviewer', '1', 'fork', true, 'l_native', 'haiku'], ['spawn', 'home_room', 'reviewer', '2', 'fork', true, 'l_native', 'haiku'], ['spawn', 'home_room', 'worker', '3', 'fork', true, 'l_native', 'haiku']],
        'each ticked session is started in order as a copy in a room of its own, through the native harness');
      assert.deepEqual(imported.states, ['Started as copy-1', 'Started as copy-2', 'Not started: limit reached']);
      assert.equal(imported.final, '2 of 3 sessions started; the project\'s agent or spawn-rate limit was reached, import the rest later.');
      assert.deepEqual(imported.afterRun, { ticked: 1, locked: [true, true, false] }, 'started sessions are locked, the unstarted one stays ticked for another try');
      // A Docker Claude harness that shares the host's Claude home is offered next to the native one; one that does not say so is not.
      assert.deepEqual(await first.evaluate(async projectID => {
        const { ClaudeSessionImport } = await import('/js/pages/claude-import/page.js');
        const entry = (launcher, label, containerized, shares) => ({ tool: 'claude', variant: containerized ? 'docker' : 'native', launcher_id: launcher, containerized, shares_claude_home: shares, available: true, label, models: [{ id: 'haiku', default: true }] });
        const source = { roles: async () => [{ role_key: 'worker', label: 'Worker' }], claudeSessions: async () => ({ sessions: [] }), spawn: async () => ({}),
          catalog: async () => ({ tools: [entry('l_native', 'Claude Code', false, false), entry('l_shared', 'Claude Code (shared home)', true, true), entry('l_own', 'Claude Code (own home)', true, false)] }) };
        const dialog = new ClaudeSessionImport(source);
        await dialog.open({ projectID, homeRoomID: 'home_room' });
        for (let i = 0; i < 100 && !document.querySelectorAll('#cc-import-harness option').length; i++) await new Promise(resolve => setTimeout(resolve, 20));
        const labels = [...document.querySelectorAll('#cc-import-harness option')].map(option => option.textContent);
        document.querySelector('#cc-import-close').click();
        return labels;
      }, actualProjectID), ['Claude Code', 'Claude Code (shared home)'], 'the import offers a Docker Claude harness only when it shares the host Claude home');
      // While an import runs its dialog cannot be closed (a reopened one could mix projects): Close is disabled and Escape is refused, both
      // until the import is done.
      const guarded = await first.evaluate(async projectID => {
        const { ClaudeSessionImport } = await import('/js/pages/claude-import/page.js');
        let release; const gate = new Promise(resolve => { release = resolve; });
        const rooms = [];
        const source = {
          roles: async () => [{ role_key: 'worker', label: 'Worker' }],
          catalog: async () => ({ tools: [{ tool: 'claude', variant: 'native', launcher_id: 'l_native', containerized: false, available: true, label: 'Claude Code', models: [{ id: 'haiku', default: true }] }] }),
          claudeSessions: async () => ({ sessions: [{ id: 'bbbbbbbb-0000-4000-8000-000000000001', title: 'Only one', cwd: '/p', modified_at: '2026-10-24T10:00:00Z', size: 1 }] }),
          spawn: async room => { rooms.push(room); await gate; return { slug: 'copy-1' }; },
        };
        const dialog = new ClaudeSessionImport(source);
        await dialog.open({ projectID, homeRoomID: 'room_one' });
        const waitFor = async test => { for (let i = 0; i < 100 && !test(); i++) await new Promise(resolve => setTimeout(resolve, 20)); };
        await waitFor(() => document.querySelectorAll('#cc-import-list .cc-import-row').length === 1);
        document.querySelector('#cc-import-all').click();
        await new Promise(resolve => setTimeout(resolve, 0)); // the page draws what the click changed
        document.querySelector('#cc-import-run').click();
        await waitFor(() => rooms.length === 1);
        dialog.homeRoomID = 'room_two'; // a reopen for another project changes what the dialog holds; the running import must not follow
        const cancel = new Event('cancel', { cancelable: true });
        document.querySelector('#cc-import-dialog').dispatchEvent(cancel);
        const during = { closeDisabled: document.querySelector('#cc-import-close').disabled, escapeRefused: cancel.defaultPrevented, open: document.querySelector('#cc-import-dialog').open,
          rolesLocked: [...document.querySelectorAll('#cc-import-list select')].every(select => select.disabled) };
        // Opening it again for another project is refused, and a close from outside (it is a modal dialog, so the browser can also close it) is undone.
        await dialog.open({ projectID: 'another_project', homeRoomID: 'room_other' });
        during.rows = document.querySelectorAll('#cc-import-list .cc-import-row').length;
        document.querySelector('#cc-import-dialog').close();
        await new Promise(resolve => setTimeout(resolve, 50));
        during.reopened = document.querySelector('#cc-import-dialog').open;
        release();
        await waitFor(() => /started/.test(document.querySelector('#cc-import-status').textContent));
        const after = { closeDisabled: document.querySelector('#cc-import-close').disabled, escapeRefused: (() => { const again = new Event('cancel', { cancelable: true }); document.querySelector('#cc-import-dialog').dispatchEvent(again); return again.defaultPrevented; })() };
        document.querySelector('#cc-import-close').click();
        return { during, after, rooms };
      }, actualProjectID);
      assert.deepEqual(guarded, { during: { closeDisabled: true, escapeRefused: true, open: true, rolesLocked: true, rows: 1, reopened: true }, after: { closeDisabled: false, escapeRefused: false }, rooms: ['room_one'] }, 'a running import keeps its dialog open and its room');
    }
    console.log('UI smoke part claude passed');
  } finally {
    await ctx.close();
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
