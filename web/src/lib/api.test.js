import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  base,
  setSelfId,
  ApiError,
  renameNode,
  setVolume,
  setOutputDelay,
  setDisabled,
  pause,
  resume,
  follow,
  unfollow,
  makeMaster,
  play,
  playOnNode,
  getMedia,
  setGroupSettings,
} from "./api.js";

const SELF = "11111111111111111111111111111111";
const REMOTE = "22222222222222222222222222222222";

function mockFetch(status, body) {
  return vi.fn(async () => ({
    ok: status >= 200 && status < 300,
    status,
    statusText: "x",
    text: async () => (body === undefined ? "" : JSON.stringify(body)),
  }));
}

beforeEach(() => {
  setSelfId(SELF);
});

describe("base", () => {
  it("local for empty / self", () => {
    expect(base("")).toBe("/api");
    expect(base(undefined)).toBe("/api");
    expect(base(SELF)).toBe("/api");
  });
  it("proxied for remote", () => {
    expect(base(REMOTE)).toBe("/api/" + REMOTE);
  });
});

describe("node actions", () => {
  it("renameNode remote → PATCH /api/<remote>/node {name}", async () => {
    global.fetch = mockFetch(200, {});
    await renameNode(REMOTE, "kitchen");
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/node");
    expect(opts.method).toBe("PATCH");
    expect(JSON.parse(opts.body)).toEqual({ name: "kitchen" });
  });
  it("setVolume sends {volume}", async () => {
    global.fetch = mockFetch(200, {});
    await setVolume(REMOTE, 0.5);
    expect(JSON.parse(global.fetch.mock.calls[0][1].body)).toEqual({
      volume: 0.5,
    });
  });
  it("setOutputDelay sends {outputDelayMs}", async () => {
    global.fetch = mockFetch(200, {});
    await setOutputDelay(REMOTE, 120);
    expect(JSON.parse(global.fetch.mock.calls[0][1].body)).toEqual({
      outputDelayMs: 120,
    });
  });
  it("setDisabled sends {disabled} (D40)", async () => {
    global.fetch = mockFetch(200, {});
    await setDisabled(REMOTE, ["playback", "opus"]);
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/node");
    expect(opts.method).toBe("PATCH");
    expect(JSON.parse(opts.body)).toEqual({ disabled: ["playback", "opus"] });
  });
});

describe("play/pause (D39)", () => {
  it("pause posts to master /pause", async () => {
    global.fetch = mockFetch(200, {});
    await pause(REMOTE);
    expect(global.fetch.mock.calls[0][0]).toBe("/api/" + REMOTE + "/pause");
    expect(global.fetch.mock.calls[0][1].method).toBe("POST");
  });
  it("resume posts to master /resume", async () => {
    global.fetch = mockFetch(200, {});
    await resume(REMOTE);
    expect(global.fetch.mock.calls[0][0]).toBe("/api/" + REMOTE + "/resume");
  });
});

describe("membership", () => {
  it("follow posts {target}", async () => {
    global.fetch = mockFetch(200, {});
    await follow(SELF, REMOTE);
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/follow");
    expect(JSON.parse(opts.body)).toEqual({ target: REMOTE });
  });
  it("add-node-to-group: follow(X, master) → POST /api/<X>/follow {target:master}", async () => {
    // GroupCard's "Add node…" folds node X into a group by following X onto
    // that group's master, issued via the proxy on X.
    const MASTER = "33333333333333333333333333333333";
    global.fetch = mockFetch(200, {});
    await follow(REMOTE, MASTER);
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/follow");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({ target: MASTER });
  });
  it("unfollow posts to acting node", async () => {
    global.fetch = mockFetch(200, {});
    await unfollow(REMOTE);
    expect(global.fetch.mock.calls[0][0]).toBe("/api/" + REMOTE + "/unfollow");
  });
  it("makeMaster posts {node} to /group/master", async () => {
    global.fetch = mockFetch(200, {});
    await makeMaster(REMOTE, REMOTE);
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/group/master");
    expect(JSON.parse(opts.body)).toEqual({ node: REMOTE });
  });
});

describe("media + play", () => {
  it("play posts {uri}", async () => {
    global.fetch = mockFetch(200, {});
    await play(REMOTE, "file:jazz.flac");
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/play");
    expect(JSON.parse(opts.body)).toEqual({ uri: "file:jazz.flac" });
  });
  it("getMedia GETs /media", async () => {
    global.fetch = mockFetch(200, [{ path: "a.wav" }]);
    const list = await getMedia(REMOTE);
    expect(global.fetch.mock.calls[0][0]).toBe("/api/" + REMOTE + "/media");
    expect(list).toEqual([{ path: "a.wav" }]);
  });
});

describe("playOnNode (takeover-then-play)", () => {
  const MASTER = "33333333333333333333333333333333";
  const GID = "44444444444444444444444444444444";

  // snapshot helper: REMOTE is a member of a group mastered by `master`.
  const snapWithMaster = (master) => ({
    nodes: [],
    groups: [{ id: GID, master, members: [MASTER, REMOTE] }],
  });

  it("already master → plays directly, no make-master", async () => {
    global.fetch = mockFetch(200, {});
    const getSnapshot = () => snapWithMaster(REMOTE);
    await playOnNode(REMOTE, "file:x.flac", getSnapshot);
    // exactly one fetch: the /play.
    expect(global.fetch).toHaveBeenCalledTimes(1);
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + REMOTE + "/play");
    expect(JSON.parse(opts.body)).toEqual({ uri: "file:x.flac" });
  });

  it("mismatch → make-master, poll until master==picked, then play", async () => {
    global.fetch = mockFetch(200, {});
    // first snapshot: REMOTE is a follower (MASTER masters). After takeover the
    // poll returns a snapshot where REMOTE is the master.
    let polls = 0;
    const getSnapshot = () => {
      // initial read sees the old master; subsequent polls see the new one.
      if (polls++ === 0) return snapWithMaster(MASTER);
      return snapWithMaster(REMOTE);
    };
    await playOnNode(REMOTE, "file:y.flac", getSnapshot, { pollMs: 1 });

    // two fetches in order: make-master then play.
    expect(global.fetch).toHaveBeenCalledTimes(2);
    const [p0] = global.fetch.mock.calls[0];
    const [p1, o1] = global.fetch.mock.calls[1];
    expect(p0).toBe("/api/" + REMOTE + "/group/master");
    expect(JSON.parse(global.fetch.mock.calls[0][1].body)).toEqual({
      node: REMOTE,
    });
    expect(p1).toBe("/api/" + REMOTE + "/play");
    expect(JSON.parse(o1.body)).toEqual({ uri: "file:y.flac" });
  });

  it("timeout → make-master only, no play, throws", async () => {
    global.fetch = mockFetch(200, {});
    // the picked node never becomes master.
    const getSnapshot = () => snapWithMaster(MASTER);
    await expect(
      playOnNode(REMOTE, "file:z.flac", getSnapshot, {
        pollMs: 1,
        timeoutMs: 5,
      }),
    ).rejects.toMatchObject({ message: "takeover timed out" });
    // make-master was issued; play was NOT.
    const paths = global.fetch.mock.calls.map((c) => c[0]);
    expect(paths).toContain("/api/" + REMOTE + "/group/master");
    expect(paths).not.toContain("/api/" + REMOTE + "/play");
  });
});

describe("group settings (D47)", () => {
  const MASTER = "33333333333333333333333333333333";
  it("setGroupSettings POSTs the full trio to the master /group/settings", async () => {
    global.fetch = mockFetch(204);
    await setGroupSettings(MASTER, {
      codec: "pcm",
      transport: "tcp",
      bufferMs: 250,
    });
    const [path, opts] = global.fetch.mock.calls[0];
    expect(path).toBe("/api/" + MASTER + "/group/settings");
    expect(opts.method).toBe("POST");
    expect(JSON.parse(opts.body)).toEqual({
      codec: "pcm",
      transport: "tcp",
      bufferMs: 250,
    });
  });
});

describe("errors", () => {
  it("non-2xx with {error} throws ApiError carrying status+message", async () => {
    global.fetch = mockFetch(409, { error: "not a master" });
    await expect(play(REMOTE, "file:x")).rejects.toMatchObject({
      status: 409,
      message: "not a master",
    });
  });
  it("ApiError instanceof Error", async () => {
    global.fetch = mockFetch(500, { message: "boom" });
    let err;
    try {
      await unfollow(REMOTE);
    } catch (e) {
      err = e;
    }
    expect(err).toBeInstanceOf(ApiError);
    expect(err.message).toBe("boom");
  });
});
