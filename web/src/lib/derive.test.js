import { describe, it, expect } from "vitest";
import {
  ZERO_ID,
  nodeById,
  nameOf,
  isIdle,
  joinTargets,
  groupLabel,
  groupNameIsDerived,
  selfNode,
  addTargets,
  deriveRole,
  playerZone,
} from "./derive.js";

const A = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const B = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const C = "cccccccccccccccccccccccccccccccc";

function snap() {
  const caps = { capabilities: { playback: true } };
  return {
    nodes: [
      { id: A, name: "alice", alive: true, following: ZERO_ID, ...caps },
      { id: B, name: "bob", alive: true, following: A, ...caps },
      { id: C, name: "carol", alive: true, following: ZERO_ID, ...caps },
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

describe("isIdle", () => {
  it("true iff following === ZERO_ID (player plays nothing)", () => {
    expect(isIdle({ following: ZERO_ID })).toBe(true);
    expect(isIdle({ following: A })).toBe(false);
    expect(isIdle(undefined)).toBe(false);
  });
});

describe("joinTargets", () => {
  it("any other alive node (a zone), minus self and current target", () => {
    const s = snap();
    const bob = nodeById(s, B); // bob's target is A → exclude self(B) + A
    expect(joinTargets(s, bob).map((n) => n.id)).toEqual([C]);
  });
  it("excludes dead nodes", () => {
    const s = snap();
    s.nodes[2].alive = false; // carol dead
    const bob = nodeById(s, B);
    expect(joinTargets(s, bob)).toEqual([]);
  });
  it("includes followers as zones now (every node masters its own group)", () => {
    const s = snap();
    const alice = nodeById(s, A); // target ZERO → exclude only self(A)
    expect(joinTargets(s, alice).map((n) => n.id)).toEqual([B, C]);
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

describe("groupNameIsDerived", () => {
  it("reflects the server nameDerived flag (D42)", () => {
    expect(groupNameIsDerived({ name: "kitchen", nameDerived: false })).toBe(
      false,
    );
    expect(
      groupNameIsDerived({ name: "bedroom + kitchen", nameDerived: true }),
    ).toBe(true);
    expect(groupNameIsDerived(undefined)).toBe(false);
    expect(groupNameIsDerived({ name: "x" })).toBe(false); // absent flag → not derived
  });
});

describe("selfNode", () => {
  it("resolves self", () => {
    expect(selfNode(snap(), A).name).toBe("alice");
  });
});

describe("deriveRole", () => {
  // self's role follows its own `following` (the player target), D49+.
  const view = (following, groups) => ({
    nodes: [{ id: A, following }],
    groups: groups || [],
  });
  it("idle when following Zero (player plays nothing)", () => {
    expect(deriveRole(view(ZERO_ID), A)).toBe("idle");
  });
  it("solo when playing its own group alone", () => {
    expect(deriveRole(view(A, [{ master: A, members: [A] }]), A)).toBe("solo");
  });
  it("master when its own group has other players", () => {
    expect(deriveRole(view(A, [{ master: A, members: [A, B] }]), A)).toBe(
      "master",
    );
  });
  it("follower when playing another node's group", () => {
    const s = {
      nodes: [{ id: B, following: A }],
      groups: [{ master: A, members: [A, B] }],
    };
    expect(deriveRole(s, B)).toBe("follower");
  });
  it("falls back when self isn't resolvable", () => {
    expect(deriveRole({ nodes: [], groups: [] }, C, "solo")).toBe("solo");
    expect(deriveRole(undefined, A)).toBe("idle");
    expect(deriveRole({ nodes: [] }, "")).toBe("idle");
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
  it("excludes nodes that can't play (e.g. --role master, playback:false)", () => {
    const s = snap();
    s.nodes[2].capabilities = { playback: false }; // carol is a no-playback master
    const group = { id: A, master: A, members: [A, B] };
    expect(addTargets(s, group)).toEqual([]); // carol no longer offered as a player
  });
  it("empty for null inputs", () => {
    expect(addTargets(undefined, { members: [] })).toEqual([]);
    expect(addTargets(snap(), null)).toEqual([]);
  });
});

describe("playerZone", () => {
  const D = "dddddddddddddddddddddddddddddddd";
  function zsnap() {
    const caps = { capabilities: { playback: true } };
    return {
      nodes: [
        { id: A, name: "alice", alive: true, following: A, ...caps }, // masters Living Room
        { id: B, name: "bob", alive: true, following: A, ...caps }, // in Living Room
        { id: C, name: "carol", alive: true, following: ZERO_ID, ...caps }, // idle
        { id: D, name: "dave", alive: true, following: D, ...caps }, // solo own zone
      ],
      groups: [
        { id: A, master: A, name: "Living Room", members: [A, B], nameDerived: false },
        { id: D, master: D, name: "dave", members: [D], nameDerived: true },
      ],
    };
  }
  it("idle when following nothing", () => {
    expect(playerZone(zsnap(), nodeById(zsnap(), C))).toBe("idle");
  });
  it("idle for a solo own zone with no other players", () => {
    expect(playerZone(zsnap(), nodeById(zsnap(), D))).toBe("idle");
  });
  it("names the room a follower currently plays in", () => {
    expect(playerZone(zsnap(), nodeById(zsnap(), B))).toBe("Living Room");
  });
  it("names the room a master currently anchors (has other members)", () => {
    expect(playerZone(zsnap(), nodeById(zsnap(), A))).toBe("Living Room");
  });
  it("idle for null/missing node", () => {
    expect(playerZone(zsnap(), null)).toBe("idle");
    expect(playerZone(zsnap(), undefined)).toBe("idle");
  });
});
