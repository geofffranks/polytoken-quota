#!/usr/bin/env bash
# Execute the exact inline reporter JavaScript and replay-seed validation from the workflow.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
ruby "$root/scripts/test-property-capture.rb"
node - "$root/.github/workflows/property-discovery.yml" <<'NODE'
const fs = require('node:fs');
const vm = require('node:vm');
const assert = require('node:assert/strict');
const { spawnSync } = require('node:child_process');
const file = process.argv[2];
const source = fs.readFileSync(file, 'utf8');
const lines = source.split(/\r?\n/);
const start = lines.findIndex(line => line.trim() === 'script: |');
assert.notEqual(start, -1, 'workflow has inline reporter script');
const js = [];
for (let i = start + 1; i < lines.length; i++) {
  const line = lines[i];
  if (line.trim() && !/^\s{12,}/.test(line)) break;
  js.push(line.replace(/^ {12}/, ''));
}
const script = js.join('\n');
assert.match(script, /github\.rest\.issues\.listForRepo/);
assert.match(script, /github\.rest\.issues\.listComments/);
const seedRun = lines.find(line => line.trim().startsWith('run: if [[ -n "$REPLAY_SEED"'));
assert.ok(seedRun, 'workflow replay validation exists');
const seedValidation = seedRun.trim().slice('run: '.length);
for (const seed of ['', '1', '123456789012345678', '0', '-1', '1; touch /tmp/unsafe', '$(id)', ' 2', '9999999999999999999']) {
  const result = spawnSync('bash', ['-c', seedValidation], { encoding: 'utf8', env: { ...process.env, REPLAY_SEED: seed } });
  const valid = seed === '' || /^[1-9][0-9]{0,17}$/.test(seed);
  assert.equal(result.status, valid ? 0 : 2, `seed validation for ${JSON.stringify(seed)}`);
}
const baseContext = {repo:{owner:'geofffranks',repo:'polytoken-quota'},ref:'refs/heads/main',eventName:'schedule',sha:'0123456789abcdef0123456789abcdef01234567',runId:12345};
const marker = '<!-- polytoken-quota-property-discovery:filesystem:linux-x64:v1 -->';
const fixedIssueBody = `${marker}\nAutomated triage issue; details are maintained in its marked comment.`;
async function run({env={}, issues=[], comments=[], apiFailAt='', context=baseContext}={}) {
  const calls=[]; const pages=[]; let nextIssue=100; let nextComment=1000;
  const github={rest:{issues:{
    async listForRepo(args){calls.push(['listIssues',args.page]);pages.push(args.page);if(apiFailAt==='listIssues')throw Error('synthetic api error');return {data:issues.filter(x=>x.page===args.page&&x.state!=='closed')};},
    async create(args){calls.push(['createIssue',args]);if(apiFailAt==='createIssue')throw Error('synthetic api error');const data={number:++nextIssue,user:{login:'github-actions[bot]'},body:args.body};issues.push({page:1,...data});return {data};},
    async listComments(args){calls.push(['listComments',args.issue_number]);if(apiFailAt==='listComments')throw Error('synthetic api error');return {data:comments};},
    async createComment(args){calls.push(['createComment',args]);if(apiFailAt==='createComment')throw Error('synthetic api error');comments.push({id:++nextComment,user:{login:'github-actions[bot]'},body:args.body});},
    async updateComment(args){calls.push(['updateComment',args]);if(apiFailAt==='updateComment')throw Error('synthetic api error');},
    async update(){throw Error('issue body must never be updated');},
  }}};
  const core={summary:{addHeading(){},addList(){},async write(){}},setFailed(message){this.failed=message;}};
  try { await vm.runInNewContext(`(async()=>{${script}\n})()`,{context,github,core,process:{env:{GH_SERVER_URL:'https://github.com',...env}}},{timeout:3000}); }
  catch(error){core.failed=error.message;}
  return {calls,pages,failed:core.failed||null};
}
(async()=>{
  const success=await run(); assert.deepEqual(success.calls,[],'successful/skipped suites do not report');
  const canceled=await run({env:{FS_LINUX:'cancelled'}}); assert.deepEqual(canceled.calls,[],'cancelled suite does not report as bug');
  const skipped=await run({env:{FS_LINUX:'skipped'}}); assert.deepEqual(skipped.calls,[],'skipped suite does not report as bug');
  const fresh=await run({env:{FS_LINUX:'failure'}});
  const issueCall=fresh.calls.find(c=>c[0]==='createIssue'); const commentCall=fresh.calls.find(c=>c[0]==='createComment');
  assert.ok(issueCall&&commentCall,'failure creates fixed issue then marked comment');
  assert.equal(issueCall[1].body,fixedIssueBody,'issue body is immutable fixed text');
  assert.match(commentCall[1].body,/inconclusive/); assert.match(commentCall[1].body,/Tested revision: [0-9a-f]{40}/);
  assert.doesNotMatch(commentCall[1].body,/assertion|counterexample contents|seed corpus|task data/);
  assert.equal(fresh.calls.some(c=>c[0]==='update'),false,'issue text is never updated');
  const human=await run({env:{FS_LINUX:'failure'},issues:[{page:1,number:1,user:{login:'human'},body:marker}]});
  assert.ok(human.calls.some(c=>c[0]==='createIssue'),'human-authored marker cannot claim ownership');
  const forged=await run({env:{FS_LINUX:'failure'},issues:[{page:1,number:2,user:{login:'github-actions[bot]'},body:`${marker}\nHuman-edit added`}]});
  assert.ok(forged.calls.some(c=>c[0]==='createIssue'),'human-edited issue body is not treated as reporter-owned');
  const ownedComment={id:42,user:{login:'github-actions[bot]'},body:`${marker}\nNeeds triage: the discovery job failed or was inconclusive; inspect the run to distinguish a property failure from timeout or infrastructure trouble.\nSuite: filesystem\nPlatform: linux-x64\nTested revision: 0123456789abcdef0123456789abcdef01234567\nRecent runs:\n${Array.from({length:9},(_,i)=>`- ${String(i).padStart(40,'0')}: https://github.com/geofffranks/polytoken-quota/actions/runs/${i+1}`).join('\n')}`};
  const priorSha='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa';
  const priorBody=ownedComment.body.split('\n').slice(0,7).join('\n').replace('Tested revision: 0123456789abcdef0123456789abcdef01234567',`Tested revision: ${priorSha}`).replace(/- [0-9a-f]{40}: https:\/\/github\.com\/geofffranks\/polytoken-quota\/actions\/runs\/[0-9]+/,`- ${priorSha}: https://github.com/geofffranks/polytoken-quota/actions/runs/111`);
  const malformed={id:41,user:{login:'github-actions[bot]'},body:`${marker}\nmalformed`};
  let comments=[malformed,{...ownedComment,id:42,body:priorBody}];
  for(const [sha,runId] of [['bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',12345],['cccccccccccccccccccccccccccccccccccccccc',12346],['dddddddddddddddddddddddddddddddddddddddd',12347]]) {
    const result=await run({env:{FS_LINUX:'failure'},context:{...baseContext,sha,runId},issues:[{page:1,number:42,user:{login:'github-actions[bot]'},body:fixedIssueBody}],comments});
    const update=result.calls.find(c=>c[0]==='updateComment'); assert.ok(update,'valid owned comment after malformed comment is updated');
    assert.equal(update[1].comment_id,42,'malformed preceding comment is ignored');
    assert.match(update[1].body,new RegExp(`Tested revision: ${sha}`),'current revision is recorded');
    assert.ok(update[1].body.includes(`- ${sha}: https://github.com/geofffranks/polytoken-quota/actions/runs/${runId}`),'current run is first');
    assert.ok(update[1].body.includes(`- ${priorSha}: https://github.com/geofffranks/polytoken-quota/actions/runs/111`),'prior run link is retained');
    comments=[malformed,{id:42,user:{login:'github-actions[bot]'},body:update[1].body}];
  }
  const boundedBody=comments[1].body;
  assert.equal((boundedBody.match(/^- /gm)||[]).length,4,'successive history remains bounded and preserves links');
  const closed=await run({env:{FS_LINUX:'failure'},issues:[{page:1,number:44,state:'closed',user:{login:'github-actions[bot]'},body:fixedIssueBody}]});
  assert.ok(closed.calls.some(c=>c[0]==='createIssue'),'closed issue is not reopened; later failure creates a fresh issue');
  assert.ok(!closed.calls.some(c=>c[0]==='updateComment'),'closed issue history is not changed');
  const tampered=await run({env:{FS_LINUX:'failure'},issues:[{page:1,number:43,user:{login:'github-actions[bot]'},body:fixedIssueBody}],comments:[{id:43,user:{login:'github-actions[bot]'},body:`${ownedComment.body}\nHuman additions must not be copied`}]});
  assert.ok(tampered.calls.some(c=>c[0]==='createComment'),'malformed bot comment gets a fresh fixed comment');
  assert.ok(!tampered.calls.some(c=>c[0]==='updateComment'),'malformed comment is not copied or overwritten');
  const paged=await run({env:{FS_LINUX:'failure'},issues:[...Array.from({length:100},(_,i)=>({page:1,number:i+1,user:{login:'human'},body:''})),{page:2,number:101,user:{login:'github-actions[bot]'},body:fixedIssueBody}]});
  assert.deepEqual(paged.pages,[1,2]); assert.ok(paged.calls.some(c=>c[0]==='createComment'));
  for(const context of [ {...baseContext,ref:'refs/heads/feature'}, {...baseContext,eventName:'pull_request'}, {...baseContext,repo:{owner:'attacker',repo:'repo'}}, {...baseContext,sha:'bad'}, {...baseContext,runId:'x'} ]) {
    const rejected=await run({env:{FS_LINUX:'failure'},context}); assert.ok(rejected.failed); assert.equal(rejected.calls.length,0,'bad provenance makes no API calls');
  }
  const wrongServer=await run({env:{FS_LINUX:'failure',GH_SERVER_URL:'https://evil.invalid'}}); assert.ok(wrongServer.failed); assert.equal(wrongServer.calls.length,0);
  for(const point of ['listIssues','createIssue','listComments','createComment','updateComment']) {
    const existing = point==='updateComment' ? {issues:[{page:1,number:60,user:{login:'github-actions[bot]'},body:fixedIssueBody}],comments:[{...ownedComment,id:60}]} : {};
    const result=await run({env:{FS_LINUX:'failure',RAPID:'failure'},apiFailAt:point,...existing}); assert.match(result.failed,/issue reports failed/); assert.ok(result.calls.length>0);
  }
  const independent=await run({env:{FS_LINUX:'failure',RAPID:'failure'},apiFailAt:'listComments'});
  assert.equal(independent.calls.filter(c=>c[0]==='listIssues').length,2,'one category failure does not suppress later category');
  console.log('property reporter workflow fixtures passed');
})().catch(error=>{console.error(error);process.exitCode=1;});
NODE
