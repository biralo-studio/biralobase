export interface RemoteObject {
  readonly $type: string;
  $get<T = unknown>(field: string): Promise<T>;
  $set(field: string, value: unknown): Promise<void>;
}
export interface RecordRef extends RemoteObject {
  readonly id: Promise<string>;
  get(field: string): Promise<unknown>;
  getString(field: string): Promise<string>;
  getBool(field: string): Promise<boolean>;
  getInt(field: string): Promise<number>;
  set(field: string, value: unknown): Promise<void>;
  publicExport(): Promise<Record<string, unknown>>;
}
export interface AppRef extends RemoteObject {
  newRecord(collection: string): Promise<RecordRef>;
  findRecordById(collection: string, id: string): Promise<RecordRef>;
  save(record: RecordRef): Promise<void>;
  delete(record: RecordRef): Promise<void>;
}
export interface HookEvent extends RemoteObject {
  readonly record: Promise<RecordRef>;
  readonly app: Promise<AppRef>;
  next(): Promise<void>;
}
export interface FunctionContext {
  request: { method: string; url: string; headers: Record<string, string[]>; body: unknown };
  auth: (Record<string, unknown> & { id: string }) | null;
  api: { fetch<T = unknown>(method: 'GET' | 'POST' | 'PATCH' | 'DELETE', path: string, body: unknown): Promise<{ status: number; body: T }> };
  /** Privileged operations bypass record rules. Prefer api for user requests. */
  admin: AppRef;
}
export interface BackgroundContext {
  job: { id: string; kind: 'event' | 'schedule'; target: string; attempt: number };
  event: { action?: 'create' | 'update' | 'delete'; collection?: string; record?: Record<string, unknown>; scheduledAt?: string };
  admin: AppRef;
}
export interface Application {
  hooks?: { event: string; tags?: string[]; priority?: number; handler: (event: HookEvent) => Promise<unknown> }[];
  functions?: Record<string, (ctx: FunctionContext) => Promise<{ status?: number; headers?: Record<string, string>; body?: unknown }>>;
  events?: Record<string, { collection: string; actions: ('create' | 'update' | 'delete')[]; handler: (ctx: BackgroundContext) => Promise<unknown> }>;
  schedules?: Record<string, { cron: string; handler: (ctx: BackgroundContext) => Promise<unknown> }>;
}
