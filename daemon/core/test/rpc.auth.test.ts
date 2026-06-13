/**
 * TCP token auth (plan §4 acceptance): an unauthenticated TCP request is
 * rejected ("auth required"), a bad token is "invalid", a good token serves the
 * connection, and UDS is exempt from the handshake entirely.
 */

import { afterEach, beforeEach, describe, expect, test } from "bun:test";

import { ErrorCodes } from "@autosk/sdk";

import { RpcClient, startTestDaemon, type TestDaemon } from "./rpcHarness.ts";

const TOKEN = "s3cr3t-token";

describe("TCP auth handshake", () => {
  let td: TestDaemon;
  let host: string;
  let port: number;

  beforeEach(async () => {
    td = await startTestDaemon({ tcp: { host: "127.0.0.1", port: 0 }, token: TOKEN });
    const addr = td.runtime.tcpAddress!;
    host = addr.host;
    port = addr.port;
  });
  afterEach(async () => {
    await td.cleanup();
  });

  test("an unauthenticated request is refused with 'auth required'", async () => {
    const client = await RpcClient.connectTcp(host, port);
    const frame = await client.callRaw("meta.version", null);
    expect(frame.result).toBeUndefined();
    expect(frame.error?.code).toBe(ErrorCodes.INVALID_REQUEST);
    expect(frame.error?.message).toMatch(/auth required/i);
    client.close();
  });

  test("a bad token is rejected as invalid", async () => {
    const client = await RpcClient.connectTcp(host, port);
    const frame = await client.callRaw("meta.auth", { token: "wrong" });
    expect(frame.error?.code).toBe(ErrorCodes.INVALID_REQUEST);
    expect(frame.error?.message).toMatch(/invalid/i);
    // Still unauthenticated: a follow-up non-auth request stays gated.
    const after = await client.callRaw("meta.version", null);
    expect(after.error?.message).toMatch(/auth required/i);
    client.close();
  });

  test("a good token authenticates, then the connection serves normally", async () => {
    const client = await RpcClient.connectTcp(host, port);
    const ok = await client.call<{ ok: boolean }>("meta.auth", { token: TOKEN });
    expect(ok).toEqual({ ok: true });
    const version = await client.call<{ version: string }>("meta.version", null);
    expect(typeof version.version).toBe("string");
    client.close();
  });

  test("UDS is exempt: meta.version serves without any auth", async () => {
    const uds = await td.client();
    const version = await uds.call<{ version: string }>("meta.version", null);
    expect(typeof version.version).toBe("string");
    // meta.auth over UDS is a no-op success.
    expect(await uds.call<{ ok: boolean }>("meta.auth", { token: "anything" })).toEqual({ ok: true });
  });
});
