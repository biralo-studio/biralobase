import test from 'node:test';
import assert from 'node:assert/strict';
import PocketBase from 'pocketbase';
import { EventSource } from 'eventsource';
globalThis.EventSource = EventSource;

test('official PocketBase JS SDK: auth, CRUD, rules, relations, files, realtime, batch', {timeout:30000}, async t => {
  const url = process.env.BIRALOBASE_TEST_URL;
  assert.ok(url);
  const admin = new PocketBase(url);
  await admin.collection('_superusers').authWithPassword('admin@example.com', 'compatibility-test-password');
  const users = await admin.collections.getOne('users');
  const notes = process.env.BIRALOBASE_TEST_MODE === 'production' ? await admin.collections.getOne('sdk_notes') : await admin.collections.create({ name: 'sdk_notes', type: 'base', fields: [
    { name: 'title', type: 'text', required: true },
    { name: 'owner', type: 'relation', collectionId: users.id, maxSelect: 1, required: true },
    { name: 'attachment', type: 'file', maxSelect: 1 },
  ], listRule: 'owner = @request.auth.id', viewRule: 'owner = @request.auth.id',
  createRule: "@request.auth.id != '' && @request.body.owner = @request.auth.id", updateRule: 'owner = @request.auth.id', deleteRule: 'owner = @request.auth.id' });
  const alice = await admin.collection('users').create({ email: 'alice@example.com', password: 'sdk-user-test-password', passwordConfirm: 'sdk-user-test-password' });
  await admin.collection('users').create({ email: 'bob@example.com', password: 'sdk-user-test-password', passwordConfirm: 'sdk-user-test-password' });
  const client = new PocketBase(url), other = new PocketBase(url);
  t.after(async()=>{await client.collection(notes.id).unsubscribe();});
  await client.collection('users').authWithPassword('alice@example.com', 'sdk-user-test-password');
  await other.collection('users').authWithPassword('bob@example.com', 'sdk-user-test-password');
  await client.collection('users').authRefresh();
  assert.equal(client.authStore.record.id, alice.id);
  const seen = [];
  await client.collection(notes.id).subscribe('*', event => seen.push(event));
  const form = new FormData();
  form.set('title', 'SDK note'); form.set('owner', alice.id);
  form.set('attachment', new Blob(['uploaded through unmodified SDK'], { type: 'text/plain' }), 'hello.txt');
  const record = await client.collection(notes.id).create(form);
  const expanded = await client.collection(notes.id).getOne(record.id, { expand: 'owner' });
  assert.equal(expanded.expand.owner.id, alice.id);
  assert.equal((await other.collection(notes.id).getList(1, 10)).totalItems, 0);
  await assert.rejects(other.collection(notes.id).getOne(record.id), error => error.status === 404);
  assert.equal(await (await fetch(client.files.getURL(record, record.attachment))).text(), 'uploaded through unmodified SDK');
  await client.collection(notes.id).update(record.id, { title: 'updated' });
  assert.equal((await client.collection(notes.id).getFirstListItem("title = 'updated'")).id, record.id);
  const settings = await admin.settings.getAll();
  if(process.env.BIRALOBASE_TEST_MODE !== 'production') await admin.settings.update({ batch: { ...settings.batch, enabled: true } });
  const batch = client.createBatch();
  batch.collection(notes.id).create({ title: 'batched', owner: alice.id });
  assert.equal((await batch.send())[0].status, 200);
  await client.collection(notes.id).delete(record.id);
  const deadline = Date.now() + 5000;
  while (seen.filter(e => e.record.id === record.id).length < 3 && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 25));
  assert.deepEqual(seen.filter(e => e.record.id === record.id).map(e => e.action), ['create', 'update', 'delete']);
  await client.collection(notes.id).unsubscribe();
  const health = await client.health.check(); assert.equal(health.code, 200);
});
