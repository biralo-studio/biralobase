# Application development

## Schema and generated migrations

`biralobase.schema.json` contains a format version, collection definitions, optional non-secret settings, the MCP exposure map, and its authentication collection. It uses PocketBase's collection format with stable collection and field IDs. `biralobase init` creates the initial users schema and a TypeScript project.

`schema generate` normalizes the desired schema in a temporary database, compares it with the last committed migration, and emits a full target snapshot with a parent hash. Collection services infer the actual DDL while preserving field IDs across renames. Generation replays prior migrations and the new migration before writing it. `schema check` independently replays the entire history and requires its final state to match the committed file.

Removing fields/collections or changing field types requires `schema generate --allow-destructive`. Review the resulting diff before committing. A generator cannot infer business transformations; optional `before` and `after` arrays accept individual INSERT/UPDATE/DELETE statements. Do not edit previously deployed migrations. Add a forward migration. Renaming a field means keeping its ID and changing its name; assigning a new ID is a delete/add operation.

The runtime verifies applied checksums, rejects older histories and live drift, backs up, then applies each pending migration and its ledger update in one transaction. Startup remains unready on failure. Migration history also versions settings/MCP configuration. Secrets stay outside that history.

Dashboard workflow: stop development server → `schema export` → `schema generate` → review/commit → restart. Development can adopt already-applied dashboard schema changes only when they exactly match the release; it refuses adoption if pending migrations contain data transformations. Production never adopts drift.

Schema checks validate metadata and replay DDL, not every pre-existing row against new application-level validation constraints. Test changes against a representative backup when tightening constraints or changing data meaning. This initial version does not provide an automatic importer for an arbitrary existing PocketBase deployment: migrate a copy, verify records/files/auth, and test rollback before a cutover. Never point the Turso runtime at the only copy of an existing SQLite database.

## TypeScript functions

`functions/index.ts` exports an `Application` object. The generated `functions/biralobase.d.ts` describes common APIs. `npm run build` compiles into `functions/dist`; production loads `functions/dist/index.js` and the bundle's production npm dependencies. Source changes require rebuilding the bundle, not the runtime image.

```ts
import type { Application } from './biralobase.js';

export default {
  functions: {
    notes: async (ctx) => {
      if (!ctx.auth) return { status: 401, body: { error: 'Sign in first' } };
      // Uses this caller's PocketBase auth and collection rules.
      const response = await ctx.api.fetch('GET', '/api/collections/notes/records', null);
      return { status: response.status, body: response.body };
    },
  },
  hooks: [{
    event: 'OnRecordCreate', tags: ['notes'],
    handler: async (event) => {
      const record = await event.record;
      await record.set('title', (await record.getString('title')).trim());
      await event.next();
    },
  }],
  events: {
    indexNote: {
      collection: 'notes', actions: ['create', 'update'],
      handler: async ({ job, event }) => {
        // Send job.id as the idempotency key to an external indexing service.
        console.log('index note', job.id, event.record?.id);
      },
    },
  },
  schedules: {
    daily: { cron: '0 3 * * *', handler: async ({ event }) => {
      console.log('daily task', event.scheduledAt);
    } },
  },
} satisfies Application;
```

HTTP functions are at `/api/functions/<name>`. They receive parsed JSON, request metadata, a public auth record (or null), the permission-preserving record API client, and explicit privileged `admin` access. Functions must enforce authentication/authorization appropriate to their behavior. `ctx.admin.newRecord`, `save`, `delete`, and other App methods bypass collection rules; use them only for deliberate trusted backend operations.

Hook events are live, scoped Go references. Access to Go fields and methods is asynchronous: `await event.record`, `await record.set(...)`, `await event.next()`. The bridge preserves the transaction-scoped App and nested hook chain. Call `next()` at most once, and await every Go call. References expire when their invocation ends. Go function/callback arguments (such as passing a JavaScript closure to `runInTransaction`) are not supported; low-level record hooks already participate in the caller's transaction. The reflected hook inventory is checked against the complete core App interface.

Node gets ordinary npm modules and async I/O. It runs as a private child process using a separate protocol descriptor; application stdout/stderr remain logs. A timed-out or failed worker terminates the server rather than silently removing hooks. Application code must keep logs free of secrets. The process is not an isolation boundary from trusted application developers.

Record-change jobs are inserted transactionally with the record mutation. Rollback removes both. Delivery is **at least once** with leased claims, exponential retry, and a dead-letter state after ten attempts. Use `job.id` for idempotency; a worker can crash after an external effect but before acknowledgment. Schedules use UTC cron and a persisted minute cursor, deduplicating ticks and catching up missed minutes in bounded batches. Jobs use one worker per application in this first version. A removed event/schedule handler leaves old jobs failing until explicitly reconciled.

## MCP and OAuth

Add an explicit exposure to the schema, using your collection's stable ID:

```json
{
  "authCollection": "users",
  "mcp": {
    "notescollection": {
      "name": "notes",
      "description": "Private notes belonging to the signed-in user",
      "actions": ["list", "get", "create", "update", "delete"],
      "fields": ["id", "title", "owner"]
    }
  }
}
```

Keep the rest of the schema, generate a migration, and deploy. MCP appears at `/mcp`, with tools such as `notes_list` and scopes such as `bb:notescollection:list`. Fields and actions are opt-in. Hidden fields, passwords, token keys, and wildcard/dotted fields are rejected. Input properties follow the exposure allowlist; PocketBase performs field-type and business validation.

OAuth discovery is at `/.well-known/oauth-authorization-server` and `/.well-known/oauth-protected-resource/mcp`. Dynamic client registration is `/oauth/register`; authorization, token, and revocation endpoints are `/oauth/authorize`, `/oauth/token`, and `/oauth/revoke`. Public clients use exact HTTPS or loopback redirect URIs, authorization code with S256 PKCE, and the canonical `/mcp` resource parameter. Request `offline_access` when refresh access is needed.

The consent page displays the client and scopes. Users sign in with the configured PocketBase auth collection or use an existing same-origin app login, then explicitly approve. The built-in consent password form does not orchestrate MFA or upstream social login; those users sign in through the application first. Superuser credentials/tokens are not accepted as MCP user identity.

Access tokens last 15 minutes, refresh tokens seven days. Authorization codes and refresh tokens are single use; replay revokes their grant. A fresh record lookup checks the user and token key on MCP requests, so account deletion or password/token-key changes invalidate access. Each request intersects the issued scopes with current schema exposures and goes through the existing PocketBase record API with the user's permissions. No superuser token is forwarded. Generic SQL, arbitrary filters, expansions, and hidden-field projection are not exposed as MCP tools.

Use a stable public HTTPS origin. Changing it invalidates resource-bound access; clients should register/authorize against the new origin. Newly added scopes may require client registration and consent again.
