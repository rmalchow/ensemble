import { describe, it, expect } from "vitest";
import { shortId, relTime, position, bytes, cidrList, ports } from "./fmt.js";

describe("shortId", () => {
  it("returns first 8 chars", () => {
    expect(shortId("0123456789abcdef0123456789abcdef")).toBe("01234567");
  });
  it("tolerates short input", () => {
    expect(shortId("ab")).toBe("ab");
    expect(shortId("")).toBe("");
    expect(shortId(undefined)).toBe("");
  });
});

describe("relTime", () => {
  const now = Math.floor(Date.now() / 1000);
  it("just now", () => expect(relTime(now)).toBe("just now"));
  it("seconds", () => expect(relTime(now - 12)).toBe("12s ago"));
  it("minutes", () => expect(relTime(now - 180)).toBe("3m ago"));
  it("hours", () => expect(relTime(now - 7200)).toBe("2h ago"));
  it("days", () => expect(relTime(now - 5 * 86400)).toBe("5d ago"));
  it("never on zero", () => expect(relTime(0)).toBe("never"));
});

describe("position", () => {
  it("formats m:ss zero padded", () => {
    expect(position(75)).toBe("1:15");
    expect(position(5)).toBe("0:05");
    expect(position(0)).toBe("0:00");
  });
});

describe("bytes", () => {
  it("scales units with one decimal", () => {
    expect(bytes(0)).toBe("0 B");
    expect(bytes(512)).toBe("512 B");
    expect(bytes(1536)).toBe("1.5 KB");
    expect(bytes(1024 * 1024 * 3)).toBe("3.0 MB");
  });
});

describe("cidrList", () => {
  it("joins, em-dash on empty", () => {
    expect(cidrList(["a/24", "b/64"])).toBe("a/24, b/64");
    expect(cidrList([])).toBe("—");
    expect(cidrList(undefined)).toBe("—");
  });
});

describe("ports", () => {
  it("formats the full port set compactly", () => {
    expect(
      ports({ httpPort: 8080, streamPort: 9090, sourcePort: 9200, gossipPort: 7946 }),
    ).toBe("http 8080 · stream 9090 · source 9200 · gossip 7946");
  });
  it("omits unset ports", () => {
    expect(ports({ httpPort: 8080, gossipPort: 7946 })).toBe(
      "http 8080 · gossip 7946",
    );
  });
  it("empty for missing node", () => {
    expect(ports(undefined)).toBe("");
    expect(ports({})).toBe("");
  });
});
