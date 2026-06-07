import { describe, it, expect } from "vitest";
import { entriesFor, crumbs, parentDir, joinDir } from "./tree.js";

// a small flat media listing, intentionally out of order and mixed-case.
function files() {
  return [
    { path: "albums/Xyz/02 b.mp3", name: "02 b.mp3", sizeBytes: 2, modTime: 2 },
    { path: "albums/Xyz/01 a.mp3", name: "01 a.mp3", sizeBytes: 1, modTime: 1 },
    { path: "albums/abc/track.flac", name: "track.flac", sizeBytes: 3, modTime: 3 },
    { path: "Ztop.mp3", name: "Ztop.mp3", sizeBytes: 4, modTime: 4 },
    { path: "anthem.wav", name: "anthem.wav", sizeBytes: 5, modTime: 5 },
  ];
}

describe("entriesFor", () => {
  it("lists root folders (with contained counts) then root files, folders first", () => {
    const { folders, files: f } = entriesFor(files(), "");
    expect(folders).toEqual([{ name: "albums", count: 3 }]);
    // root files sorted case-insensitively: anthem before Ztop.
    expect(f.map((x) => x.name)).toEqual(["anthem.wav", "Ztop.mp3"]);
  });

  it("descends into a nested directory", () => {
    const { folders, files: f } = entriesFor(files(), "albums");
    // both subfolders, case-insensitive sort: abc before Xyz.
    expect(folders).toEqual([
      { name: "abc", count: 1 },
      { name: "Xyz", count: 2 },
    ]);
    expect(f).toEqual([]);
  });

  it("lists files in a leaf directory, sorted case-insensitively", () => {
    const { folders, files: f } = entriesFor(files(), "albums/Xyz");
    expect(folders).toEqual([]);
    expect(f.map((x) => x.name)).toEqual(["01 a.mp3", "02 b.mp3"]);
  });

  it("treats '/', trailing and leading slashes as the same dir", () => {
    expect(entriesFor(files(), "/albums/")).toEqual(
      entriesFor(files(), "albums"),
    );
    expect(entriesFor(files(), "/")).toEqual(entriesFor(files(), ""));
  });

  it("counts files anywhere beneath a folder", () => {
    const deep = [
      { path: "a/b/c/1.mp3", name: "1.mp3" },
      { path: "a/b/2.mp3", name: "2.mp3" },
      { path: "a/3.mp3", name: "3.mp3" },
    ];
    const { folders, files: f } = entriesFor(deep, "");
    expect(folders).toEqual([{ name: "a", count: 3 }]);
    expect(f).toEqual([]);
  });

  it("is robust to empty / malformed input", () => {
    expect(entriesFor([], "")).toEqual({ folders: [], files: [] });
    expect(entriesFor(undefined, "")).toEqual({ folders: [], files: [] });
    expect(entriesFor([{ name: "x" }], "")).toEqual({
      folders: [],
      files: [],
    });
  });

  it("does not leak siblings whose name only shares a prefix", () => {
    const sib = [
      { path: "album/x.mp3", name: "x.mp3" },
      { path: "albums/y.mp3", name: "y.mp3" },
    ];
    const { files: f } = entriesFor(sib, "albums");
    expect(f.map((x) => x.name)).toEqual(["y.mp3"]);
  });
});

describe("crumbs", () => {
  it("returns just the root at the root", () => {
    expect(crumbs("")).toEqual([{ name: "media", dir: "" }]);
    expect(crumbs("/")).toEqual([{ name: "media", dir: "" }]);
  });

  it("builds cumulative crumbs for a nested path", () => {
    expect(crumbs("albums/xyz")).toEqual([
      { name: "media", dir: "" },
      { name: "albums", dir: "albums" },
      { name: "xyz", dir: "albums/xyz" },
    ]);
  });
});

describe("parentDir", () => {
  it("walks one level up, stopping at root", () => {
    expect(parentDir("albums/xyz")).toBe("albums");
    expect(parentDir("albums")).toBe("");
    expect(parentDir("")).toBe("");
  });
});

describe("joinDir", () => {
  it("appends a segment, handling the root case", () => {
    expect(joinDir("", "albums")).toBe("albums");
    expect(joinDir("albums", "xyz")).toBe("albums/xyz");
  });
});
