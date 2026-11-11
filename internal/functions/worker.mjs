import fs from 'node:fs';
import readline from 'node:readline';
import { pathToFileURL } from 'node:url';

// fd 3 is a private protocol pipe. stdout/stderr remain application logs.
const send = value => fs.writeSync(3, JSON.stringify(value) + '\n');
let sequence = 0;
const pending = new Map();
const live = new Set();
const propertyName = name => name.replace(/^[A-Z]+(?=[A-Z][a-z]|$)/, prefix => prefix.toLowerCase()).replace(/^[A-Z]/, prefix => prefix.toLowerCase());
const encode = value => {
  if (value && value[Symbol.for('biralobase.ref')]) return { $ref: value[Symbol.for('biralobase.ref')] };
  if (Array.isArray(value)) return value.map(encode);
  if (value && typeof value === 'object') return { $map: Object.fromEntries(Object.entries(value).map(([k, v]) => [k, encode(v)])) };
  if (typeof value === 'function') throw new TypeError('Callbacks cannot cross the Go bridge');
  return value;
};
const rpc = (scope, ref, op, name, args = []) => {
  if (!live.has(scope)) return Promise.reject(new Error('Invocation has ended'));
  const id = String(++sequence);
  const result = new Promise((resolve, reject) => pending.set(id, { resolve, reject, scope }));
  send({ kind: 'call', scope, id, ref, op, name, args: encode(args) });
  return result;
};
const decode = (value, scope) => {
  if (Array.isArray(value)) return value.map(v => decode(v, scope));
  if (!value || typeof value !== 'object') return value;
  if (value.$ref) {
    const methods = new Map(value.methods.map(name => [propertyName(name), name]));
    const fields = new Map(value.fields.map(name => [propertyName(name), name]));
    return new Proxy(Object.create(null), {
      get(_, name) {
        if (name === 'then') return undefined;
        if (name === Symbol.for('biralobase.ref')) return value.$ref;
        if (name === '$get') return key => rpc(scope, value.$ref, 'get', key);
        if (name === '$set') return (key, data) => rpc(scope, value.$ref, 'set', key, [data]);
        if (name === '$type') return value.type;
        if (methods.has(name)) return (...args) => rpc(scope, value.$ref, 'method', methods.get(name), args);
        if (fields.has(name)) return rpc(scope, value.$ref, 'get', fields.get(name));
        return undefined;
      },
      set() { throw new TypeError('Use await object.$set(field, value) for Go fields'); },
    });
  }
  if (value.$map) return Object.fromEntries(Object.entries(value.$map).map(([k, v]) => [k, decode(v, scope)]));
  throw new TypeError('Invalid bridge value');
};

const app = (await import(pathToFileURL(process.argv[1]).href)).default;
if (!app || typeof app !== 'object') throw new Error('Export a default application definition');
const hooks = app.hooks ?? [];
const functions = app.functions ?? {};
const events = app.events ?? {};
const schedules = app.schedules ?? {};
for (const hook of hooks) if (typeof hook.handler !== 'function') throw new Error('Every hook requires a handler');
for (const [name, fn] of Object.entries(functions)) if (typeof fn !== 'function') throw new Error(`Invalid function ${name}`);
for (const [name, fn] of Object.entries(events)) if (typeof fn.handler !== 'function') throw new Error(`Invalid event handler ${name}`);
for (const [name, fn] of Object.entries(schedules)) if (typeof fn.handler !== 'function') throw new Error(`Invalid schedule ${name}`);
send({ kind: 'ready', value: {
  hooks: hooks.map(({ event, tags = [], priority = 0 }) => ({ event, tags, priority })),
  functions: Object.keys(functions),
  events: Object.fromEntries(Object.entries(events).map(([name, { collection, actions }]) => [name, { collection, actions }])),
  schedules: Object.fromEntries(Object.entries(schedules).map(([name, { cron }]) => [name, { cron }])),
} });

readline.createInterface({ input: process.stdin, crlfDelay: Infinity }).on('line', async line => {
  try {
    const message = JSON.parse(line);
    if (message.kind === 'reply') {
      const entry = pending.get(message.id);
      if (!entry) return;
      pending.delete(message.id);
      if (message.error) entry.reject(Object.assign(new Error(message.error.message), { status: message.error.status }));
      else entry.resolve(decode(message.value, entry.scope));
      return;
    }
    if (message.kind !== 'invoke') throw new Error('Invalid protocol message');
    const { scope, category, name } = message;
    live.add(scope);
    try {
      const handler = category === 'hook' ? hooks[Number(name)]?.handler : category === 'function' ? functions[name] : category === 'event' ? events[name]?.handler : schedules[name]?.handler;
      if (!handler) throw new Error('Unknown handler');
      const value = await handler(decode(message.value, scope));
      // Fire-and-forget bridge calls can outlive a transaction. Reject them.
      if ([...pending.values()].some(p => p.scope === scope)) throw new Error('Await every Go bridge call before returning');
      send({ kind: 'result', scope, value: encode(value ?? null) });
    } catch (error) {
      send({ kind: 'result', scope, error: { message: String(error?.message ?? error), status: Number(error?.status) || 0 } });
    } finally {
      live.delete(scope);
      for (const [id, entry] of pending) {
        if (entry.scope === scope) { pending.delete(id); entry.reject(new Error('Invocation has ended')); }
      }
    }
  } catch (error) {
    console.error('Biralobase function protocol error:', error);
    process.exit(1);
  }
});
process.stdin.on('end', () => process.exit(0));
