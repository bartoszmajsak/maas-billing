const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const {test} = require('node:test');

// Execute the script shipped in the workflow, with GitHub API calls mocked.
const workflow = fs.readFileSync(path.join(__dirname, '../../../.github/workflows/rerun-help.yml'), 'utf8');
const source = workflow.split('          script: |\n')[1].replace(/^            /gm, '');
const AsyncFunction = Object.getPrototypeOf(async function() {}).constructor;
const notify = new AsyncFunction('github', 'context', source);
const marker = '<!-- maas-rerun-help -->';

async function exercise({sha = 'current', runSha = 'current', repoId = 7, comments = [], headRepository = true} = {}) {
  const calls = [];
  const pulls = [{number: 42, head: {sha, repo: {id: repoId}}}];
  const github = {
    rest: {
      pulls: {list: 'pulls'},
      issues: {
        listComments: 'comments',
        createComment: async args => calls.push(['create', args]),
        updateComment: async args => calls.push(['update', args]),
      },
    },
    paginate: async (route, args) => {
      calls.push([route, args]);
      return route === 'pulls' ? pulls : comments;
    },
  };
  const context = {
    repo: {owner: 'example', repo: 'maas'},
    payload: {workflow_run: {
      head_sha: runSha, head_branch: 'feature', pull_requests: [],
      head_repository: headRepository ? {id: 7, owner: {login: 'fork-owner'}} : null,
      html_url: 'https://github.com/example/maas/actions/runs/101',
    }},
  };
  await notify(github, context);
  return calls;
}

test('resolves fork PRs with no pull_requests payload and creates retry help', async () => {
  const calls = await exercise();
  assert.equal(calls[0][1].head, 'fork-owner:feature');
  const [kind, args] = calls.at(-1);
  assert.equal(kind, 'create');
  assert.equal(args.issue_number, 42);
  assert.ok(args.body.includes('/rerun-failed'));
  assert.ok(args.body.includes('/actions/runs/101'));
});

test('updates the existing bot comment', async () => {
  const calls = await exercise({comments: [{id: 9, user: {login: 'github-actions[bot]'}, body: marker}]});
  assert.equal(calls.at(-1)[0], 'update');
  assert.equal(calls.at(-1)[1].comment_id, 9);
});

test('does not edit a marker posted by another user', async () => {
  const calls = await exercise({comments: [{id: 9, user: {login: 'someone'}, body: marker}]});
  assert.equal(calls.at(-1)[0], 'create');
});

test('ignores old commits', async () => {
  const calls = await exercise({sha: 'new-head'});
  assert.deepEqual(calls.map(call => call[0]), ['pulls']);
});

test('ignores a different repository with the same branch and SHA', async () => {
  const calls = await exercise({repoId: 8});
  assert.deepEqual(calls.map(call => call[0]), ['pulls']);
});

test('ignores deleted head repositories', async () => {
  assert.deepEqual(await exercise({headRepository: false}), []);
});

test('identical notifications do not write again', async () => {
  const initial = await exercise();
  const body = initial.at(-1)[1].body;
  const calls = await exercise({comments: [{id: 9, user: {login: 'github-actions[bot]'}, body}]});
  assert.ok(!calls.some(call => ['create', 'update'].includes(call[0])));
});

test('different commits of one PR share a lock and update one comment', async () => {
  const groupTemplate = workflow.match(/^  group: (.+)$/m)[1];
  // Evaluate the workflow's field references so the regression covers its lock
  // identity as well as the actual notification script.
  const groupFor = run => groupTemplate.replace(
    /\$\{\{\s*github\.event\.workflow_run\.([\w.]+)\s*\}\}/g,
    (_, field) => field.split('.').reduce((value, key) => value[key], run),
  );
  const comments = [];
  let creates = 0;
  const pending = new Map();
  function schedule(sha) {
    const run = {
      head_sha: sha, head_branch: 'feature', pull_requests: [],
      head_repository: {id: 7, owner: {login: 'fork-owner'}},
      html_url: `https://github.com/example/maas/actions/runs/${sha}`,
    };
    const group = groupFor(run);
    const task = (pending.get(group) || Promise.resolve()).then(() => notify({
      rest: {
        pulls: {list: 'pulls'},
        issues: {
          listComments: 'comments',
          createComment: async ({body}) => {
            creates++;
            comments.push({id: creates, user: {login: 'github-actions[bot]'}, body});
          },
          updateComment: async ({comment_id, body}) => {
            comments.find(comment => comment.id === comment_id).body = body;
          },
        },
      },
      paginate: async route => route === 'pulls'
        ? [{number: 42, head: {sha, repo: {id: 7}}}]
        : comments.slice(),
    }, {repo: {owner: 'example', repo: 'maas'}, payload: {workflow_run: run}}));
    pending.set(group, task);
    return task;
  }
  await Promise.all([schedule('old-head'), schedule('new-head')]);
  assert.equal(creates, 1);
  assert.equal(comments.length, 1);
  assert.ok(comments[0].body.includes('/actions/runs/new-head'));
});

test('a late stale failure does not evict the pending current-head notification', async () => {
  const queueMode = workflow.match(/^  queue: (\w+)$/m)?.[1] || 'single';
  assert.ok(['single', 'max'].includes(queueMode));
  const pending = [];
  const cancelled = [];
  const processed = [];
  const calls = [];
  let release;
  const held = new Promise(resolve => { release = resolve; });
  async function processEvent(id, runSha) {
    processed.push(id);
    calls.push(...await exercise({runSha}));
  }
  // A1 holds the branch lock while B and then A2 enter the pending queue.
  // GitHub's default single-slot queue cancels B when A2 arrives.
  const active = held.then(() => processEvent('A1', 'old-head'));
  function enqueue(id, runSha) {
    if (queueMode === 'single' && pending.length) cancelled.push(pending.pop().id);
    pending.push({id, runSha});
  }
  enqueue('B', 'current');
  enqueue('A2', 'old-head');
  release();
  await active;
  for (const {id, runSha} of pending) await processEvent(id, runSha);

  assert.deepEqual(cancelled, []);
  assert.deepEqual(processed, ['A1', 'B', 'A2']);
  const creates = calls.filter(call => call[0] === 'create');
  assert.equal(creates.length, 1);
  assert.ok(creates[0][1].body.includes('commit current'));
});
