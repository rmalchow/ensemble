import { describe, it, expect } from "vitest";
import {
  ZERO_ID,
  nodeById,
  nameOf,
  isMaster,
  masterCandidates,
  joinTargets,
  groupLabel,
  selfNode,
  addTargets,
} from "./derive.js";

const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const C = "cccccccccccccccccccccccccccccccc";

function snap() {
  return {
    nodes: [
      { id: A, name: "alice", alive: true, following: ZERO_ID },
      { id: B, name: "bob", alive: true, following: A },
      { id: C, name: "carol", alive: true, following: ZERO_ID },
    ],
    groups: [],
  };
}

describe("ZERO_ID", () => {
  it("is 32 zeros", () => expect(ZERO_ID).toBe("0".repeat(32)));
});

describe("nodeById", () => {
  it("finds and misses", () => {
    expect(nodeById(snap(), A).name).toBe("alice");
    expect(nodeById(snap(), "zzzz")).toBeUndefined();
    expect(nodeById(undefined, A)).toBeUndefined();
  });
});

describe("nameOf", () => {
  it("falls back to shortId", () => {
    expect(nameOf(snap(), A)).toBe("alice");
    expect(nameOf(snap(), "deadbeef" + "0".repeat(24))).toBe("deadbeef");
  });
});

describe("isMaster", () => {
  it("true iff following === ZERO_ID", () => {
    expect(isMaster({ following: ZERO_ID })).toBe(true);
    expect(isMaster({ following: A })).toBe(false);
    expect(isMaster(undefined)).toBe(false);
  });
});

describe("masterCandidates", () => {
  it("returns all member ids", () => {
    expect(masterCandidates({ members: [A, B] })).toEqual([A, B]);
    expect(masterCandidates({})).toEqual([]);
  });
});

describe("joinTargets", () => {
  it("excludes self, current master, non-masters, dead nodes", () => {
    const s = snap();
    // bob follows alice: targets are other alive masters except alice (his master)
    const bob = nodeById(s, B);
    const t = joinTargets(s, bob).map((n) => n.id);
    expect(t).toEqual([C]); // not self(B), not master(A), carol is master
  });
  it("excludes dead masters", () => {
    const s = snap();
    s.nodes[2].alive = false; // carol dead
    const bob = nodeById(s, B);
    expect(joinTargets(s, bob)).toEqual([]);
  });
  it("alice (solo master) can join bob? bob is a follower → only carol", () => {
    const s = snap();
    const alice = nodeById(s, A);
    const t = joinTargets(s, alice).map((n) => n.id);
    expect(t).toEqual([C]);
  });
});

describe("groupLabel", () => {
  it("uses name, falls back", () => {
    expect(groupLabel({ name: "downstairs", id: A })).toBe("downstairs");
    expect(groupLabel({ name: "", id: "abcdef12" + "0".repeat(24) })).toBe(
      "Group abcdef12",
    );
  });
});

describe("selfNode", () => {
  it("resolves self", () => {
    expect(selfNode(snap(), A).name).toBe("alice");
  });
});

describe("addTargets", () => {
  it("returns alive nodes not already in the group", () => {
    const s = snap();
    const group = { id: A, master: A, members: [A, B] };
    const t = addTargets(s, group).map((n) => n.id);
    expect(t).toEqual([C]); // alice+bob are members, carol is alive & outside
  });
  it("excludes dead non-members", () => {
    const s = snap();
    s.nodes[2].alive = false; // carol dead
    const group = { id: A, master: A, members: [A, B] };
    expect(addTargets(s, group)).toEqual([]);
  });
  it("empty for null inputs", () => {
    expect(addTargets(undefined, { members: [] })).toEqual([]);
    expect(addTargets(snap(), null)).toEqual([]);
  });
});
