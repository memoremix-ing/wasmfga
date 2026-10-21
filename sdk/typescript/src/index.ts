import { WASI, File, OpenFile, ConsoleStdout } from "@bjorn3/browser_wasi_shim";

export interface Transaction {
  query(sql: string, params: string[]): Promise<string[][]>;
  execute(sql: string, params: string[]): Promise<number>;
}

export interface Request {
  operation: "schema" | "put_model" | "write" | "check";
  dialect?: "sqlite" | "turso";
  store_id?: string;
  model_id?: string;
  model?: unknown;
  check?: { tuple_key: Tuple; context?: Record<string, unknown>; contextual_tuples?: { tuple_keys: Tuple[] } };
  writes?: Tuple[];
  deletes?: Tuple[];
}
export interface Tuple {
  object: string;
  relation: string;
  user: string;
  condition?: { name: string; context?: Record<string, unknown> };
}
interface Statement { kind: "query" | "execute"; sql: string; params: string[]; expected_changes?: number }
export interface Response { allowed?: boolean; needs_results?: boolean; statements?: Statement[]; error?: string }
interface Exports extends WebAssembly.Exports {
  memory: WebAssembly.Memory;
  alloc(size: number): number;
  run(): bigint;
  _initialize(): void;
}

/** One reactor can be reused across calls; all authorization state is per request. */
export class WasmFGA {
  private constructor(private readonly exports: Exports) {}

  static async create(bytes: BufferSource | WebAssembly.Module): Promise<WasmFGA> {
    const module = bytes instanceof WebAssembly.Module ? bytes : await WebAssembly.compile(bytes);
    const wasi = new WASI(["wasmfga"], [], [new OpenFile(new File([])), ConsoleStdout.lineBuffered(console.log), ConsoleStdout.lineBuffered(console.error)], { debug: false });
    const instance = await WebAssembly.instantiate(module, { wasi_snapshot_preview1: wasi.wasiImport });
    const exports = instance.exports as Exports;
    wasi.initialize({ exports });
    return new WasmFGA(exports);
  }

  private call(request: unknown): Response {
    const input = new TextEncoder().encode(JSON.stringify(request));
    if (input.length > 16 * 1024 * 1024) throw new Error("input exceeds 16 MiB");
    const ptr = this.exports.alloc(input.length);
    if (ptr === 0) throw new Error("WASM allocation failed");
    new Uint8Array(this.exports.memory.buffer, ptr, input.length).set(input);
    const packed = this.exports.run();
    // run() may grow memory, so never retain the input view.
    const output = new Uint8Array(this.exports.memory.buffer, Number(packed >> 32n), Number(packed & 0xffffffffn));
    const response: Response = JSON.parse(new TextDecoder("utf-8", { fatal: true }).decode(output));
    if (response.error) throw new Error(`wasmfga: ${response.error}`);
    return response;
  }

  /** Borrow an active transaction. The caller MUST roll it back on any error.
   * Do not use the transaction concurrently until this promise settles.
   */
  async run(tx: Transaction, request: Request): Promise<Response> {
    if (!tx) throw new Error("an active transaction binding is required");
    const input: Request & { results?: string[][][] } = structuredClone(request);
    delete input.results;
    for (let round = 0; round < 2; round++) {
      const response = this.call(input);
      const results: string[][][] = [];
      for (const effect of response.statements ?? []) {
        if (effect.kind === "query") results.push(await tx.query(effect.sql, effect.params));
        else if (effect.kind === "execute") {
          const changed = await tx.execute(effect.sql, effect.params);
          if (effect.expected_changes !== undefined && changed !== effect.expected_changes)
            throw new Error(`expected ${effect.expected_changes} changes, got ${changed}; caller must roll back`);
        } else throw new Error("unknown SQL effect");
      }
      if (!response.needs_results) return response;
      input.results = results;
    }
    throw new Error("WASM protocol exceeded two rounds");
  }
}

/** Accepts an already-open @libsql/client transaction, including its /web client. */
export function libsqlTransaction(tx: {
  execute(statement: { sql: string; args: string[] }): Promise<{ rows: ArrayLike<{ [index: number]: unknown }>; rowsAffected: number }>;
}): Transaction {
  return {
    async query(sql, params) {
      const result = await tx.execute({ sql, args: params });
      return Array.from(result.rows, row => {
        if (typeof row[0] !== "string") throw new Error("expected a SQL TEXT column");
        return [row[0]];
      });
    },
    async execute(sql, params) { return (await tx.execute({ sql, args: params })).rowsAffected; },
  };
}

/** Adapts a SQLite/Turso connection on which the caller has already begun a transaction.
 * Works with synchronous and asynchronous prepared-statement APIs.
 */
export function preparedTransaction(connection: {
  prepare(sql: string): {
    all(...params: string[]): unknown[] | Promise<unknown[]>;
    run(...params: string[]): { changes: number | bigint } | Promise<{ changes: number | bigint }>;
  };
}): Transaction {
  return {
    async query(sql, params) {
      return (await connection.prepare(sql).all(...params)).map(row => {
        const body = (row as { body: unknown }).body;
        if (typeof body !== "string") throw new Error("expected SQL body TEXT");
        return [body];
      });
    },
    async execute(sql, params) { return Number((await connection.prepare(sql).run(...params)).changes); },
  };
}
