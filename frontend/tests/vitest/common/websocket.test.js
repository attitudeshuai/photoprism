// Pins the reconnect backoff of common/websocket.js; Sockette is mocked so no connection is opened.
import { describe, it, expect, vi, beforeAll } from "vitest";

const sockette = vi.hoisted(() => ({ opts: null }));
const viewState = vi.hoisted(() => ({ refresh: vi.fn() }));
const configState = vi.hoisted(() => ({
  update: vi.fn(() => Promise.resolve()),
}));

vi.mock("sockette", () => ({
  default: vi.fn(function (url, opts) {
    sockette.opts = opts;
  }),
}));

vi.mock("app/session", () => ({
  $config: { disconnected: { value: false }, update: configState.update },
}));

vi.mock("common/view", () => ({
  $view: {
    current: { refresh: viewState.refresh },
  },
}));

// The setup file already loaded the module with the real Sockette, so it is imported again here.
let nextReconnectTimeout, reconnectTimeout, maxReconnectTimeout, lagEvent;

beforeAll(async () => {
  vi.resetModules();
  ({ nextReconnectTimeout, reconnectTimeout, maxReconnectTimeout, lagEvent } = await import("common/websocket"));
});

beforeEach(() => {
  vi.clearAllMocks();
});

describe("common/websocket.js", () => {
  describe("nextReconnectTimeout", () => {
    it("doubles the delay up to the maximum while rate limited", () => {
      expect(nextReconnectTimeout(reconnectTimeout, "websocket.rate-limited")).toBe(10e3);
      expect(nextReconnectTimeout(40e3, "websocket.rate-limited")).toBe(maxReconnectTimeout);
      expect(nextReconnectTimeout(maxReconnectTimeout, "websocket.rate-limited")).toBe(maxReconnectTimeout);
      expect(nextReconnectTimeout(0, "websocket.rate-limited")).toBe(10e3);
    });

    it("restores the default after any other message", () => {
      expect(nextReconnectTimeout(maxReconnectTimeout, "config.updated")).toBe(reconnectTimeout);
      expect(nextReconnectTimeout(reconnectTimeout, undefined)).toBe(reconnectTimeout);
    });
  });

  describe("Sockette options", () => {
    it("updates the reconnect timeout from server messages", () => {
      const opts = sockette.opts;
      expect(opts.timeout).toBe(reconnectTimeout);

      opts.onmessage({ data: JSON.stringify({ event: "websocket.rate-limited", data: { code: 429 } }) });
      expect(opts.timeout).toBe(10e3);

      opts.onmessage({ data: JSON.stringify({ event: "websocket.rate-limited", data: { code: 429 } }) });
      expect(opts.timeout).toBe(20e3);

      opts.onmessage({ data: JSON.stringify({ event: "config.updated", data: {} }) });
      expect(opts.timeout).toBe(reconnectTimeout);
    });
  });

  describe("lag signal", () => {
    it("refetches config and reloads the active view on websocket.lag", async () => {
      const opts = sockette.opts;
      const data = { lag: true, dropped: 3, firstTopic: "photos.updated", topics: { "photos.updated": 3 } };

      opts.onmessage({ data: JSON.stringify({ event: lagEvent, data }) });

      expect(viewState.refresh).toHaveBeenCalledTimes(1);
      expect(configState.update).toHaveBeenCalledTimes(1);

      await configState.update.mock.results[0].value;
      await Promise.resolve();
    });

    it("collapses repeated lag signals while a resynchronization is running", async () => {
      const opts = sockette.opts;
      let resolveUpdate;

      configState.update.mockImplementationOnce(
        () => new Promise((resolve) => {
          resolveUpdate = resolve; 
        }),
      );

      opts.onmessage({ data: JSON.stringify({ event: lagEvent, data: { dropped: 1 } }) });
      opts.onmessage({ data: JSON.stringify({ event: lagEvent, data: { dropped: 2 } }) });
      opts.onmessage({ data: JSON.stringify({ event: lagEvent, data: { dropped: 3 } }) });

      expect(configState.update).toHaveBeenCalledTimes(1);
      expect(viewState.refresh).toHaveBeenCalledTimes(3);

      resolveUpdate();
      await Promise.resolve();

      opts.onmessage({ data: JSON.stringify({ event: lagEvent, data: { dropped: 4 } }) });
      expect(configState.update).toHaveBeenCalledTimes(2);
    });

    it("leaves ordinary messages without resynchronization", () => {
      const opts = sockette.opts;

      opts.onmessage({ data: JSON.stringify({ event: "photos.updated", data: { entities: [] } }) });

      expect(viewState.refresh).not.toHaveBeenCalled();
      expect(configState.update).not.toHaveBeenCalled();
    });
  });
});
