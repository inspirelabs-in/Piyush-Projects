import { MarkerType, type Edge, type Node } from "@xyflow/react";

import type { GraphData } from "./api";
import { extensionColor } from "./graph";

// ---------------------------------------------------------------------------
// Staged / drill-down graph model + egocentric "flow" view.
//
// Drill-down shows ONE LEVEL at a time (immediate sub-folders + files +
// modules/endpoints used there, with import edges aggregated up). Flow view
// isolates a single item and lays out its full cross-repo connections —
// importers on the left, the item in the centre, dependencies on the right.
// ---------------------------------------------------------------------------

const dirOf = (p: string): string => {
  const i = p.lastIndexOf("/");
  return i < 0 ? "" : p.slice(0, i);
};
const baseOf = (p: string): string => {
  const i = p.lastIndexOf("/");
  return i < 0 ? p : p.slice(i + 1);
};

export interface RepoIndex {
  filePaths: Map<string, string>; // file node id -> path
  fileData: Map<string, Record<string, unknown>>;
  moduleData: Map<string, Record<string, unknown>>; // module id -> data
  endpointData: Map<string, Record<string, unknown>>; // endpoint id -> data
  nodeKind: Map<string, string>; // id -> "file" | "module" | "endpoint"
  allDirs: Set<string>;
  edges: { source: string; target: string }[];
  outAdj: Map<string, string[]>; // id -> ids it points to
  inAdj: Map<string, string[]>; // id -> ids pointing to it
}

/** Pre-index a backend graph payload for fast per-level + flow queries. */
export function buildIndex(graph: GraphData): RepoIndex {
  const filePaths = new Map<string, string>();
  const fileData = new Map<string, Record<string, unknown>>();
  const moduleData = new Map<string, Record<string, unknown>>();
  const endpointData = new Map<string, Record<string, unknown>>();
  const nodeKind = new Map<string, string>();
  const allDirs = new Set<string>([""]);

  for (const n of graph.nodes) {
    const kind = String(n.data.kind);
    nodeKind.set(n.id, kind);
    if (kind === "file") {
      const path = String(n.data.path ?? n.data.label ?? "");
      if (!path) continue;
      filePaths.set(n.id, path);
      fileData.set(n.id, n.data);
      let d = dirOf(path);
      for (;;) {
        allDirs.add(d);
        if (d === "") break;
        d = dirOf(d);
      }
    } else if (kind === "module") {
      moduleData.set(n.id, n.data);
    } else if (kind === "endpoint") {
      endpointData.set(n.id, n.data);
    }
  }

  const edges: { source: string; target: string }[] = [];
  const outAdj = new Map<string, string[]>();
  const inAdj = new Map<string, string[]>();
  const seen = new Set<string>();
  for (const e of graph.edges) {
    if (e.source === e.target) continue;
    const k = `${e.source}>${e.target}`;
    if (seen.has(k)) continue;
    seen.add(k);
    edges.push({ source: e.source, target: e.target });
    (outAdj.get(e.source) ?? outAdj.set(e.source, []).get(e.source)!).push(e.target);
    (inAdj.get(e.target) ?? inAdj.set(e.target, []).get(e.target)!).push(e.source);
  }

  return { filePaths, fileData, moduleData, endpointData, nodeKind, allDirs, edges, outAdj, inAdj };
}

export const folderItemId = (dir: string): string => `agg:${dir}`;
export const folderPathFromId = (id: string): string => (id.startsWith("agg:") ? id.slice(4) : "");

function fileNodeType(path: string): string {
  return /\.(sql|prisma)$/i.test(path) ? "synapseDatabase" : "synapseFile";
}

// Layout geometry.
const CELL_W = 250;
const CELL_H = 150;
const MAX_COLS = 4;
const SIDE_GAP = 120;
const SIDE_ROW = 96;

function countsFor(index: RepoIndex, dir: string): { fileCount: number; subdirCount: number } {
  let fileCount = 0;
  for (const p of index.filePaths.values()) if (p === dir || p.startsWith(dir + "/")) fileCount++;
  let subdirCount = 0;
  for (const d of index.allDirs) if (d !== "" && dirOf(d) === dir) subdirCount++;
  return { fileCount, subdirCount };
}

/** Build a React Flow node of the right type for any id (file/module/endpoint/folder). */
function makeNode(index: RepoIndex, id: string): Node {
  if (id.startsWith("agg:")) {
    const dir = folderPathFromId(id);
    const c = countsFor(index, dir);
    return { id, type: "drillFolder", position: { x: 0, y: 0 }, data: { name: baseOf(dir) || dir, path: dir, ...c } };
  }
  const kind = index.nodeKind.get(id);
  if (kind === "module") {
    return { id, type: "synapseModule", position: { x: 0, y: 0 }, data: index.moduleData.get(id) ?? { label: id } };
  }
  if (kind === "endpoint") {
    return { id, type: "synapseEndpoint", position: { x: 0, y: 0 }, data: index.endpointData.get(id) ?? { label: id } };
  }
  const p = index.filePaths.get(id) ?? "";
  return { id, type: fileNodeType(p), position: { x: 0, y: 0 }, data: { ...index.fileData.get(id), accent: extensionColor(p) } };
}

export interface LevelView {
  nodes: Node[];
  edges: Edge[];
  filesByItem: Map<string, string[]>;
  subdirCount: number;
  fileCount: number;
  moduleCount: number;
  endpointCount: number;
}

const baseEdge = (id: string, source: string, target: string, color: string, dashed = false): Edge => ({
  id,
  source,
  target,
  type: "smoothstep",
  markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color },
  style: { stroke: color, strokeWidth: 1.5, ...(dashed ? { strokeDasharray: "5 4" } : {}) },
});

const MODULE_CAP = 22;

/** Build the React Flow view for one folder level. */
export function levelView(index: RepoIndex, path: string): LevelView {
  const subdirs: string[] = [];
  for (const d of index.allDirs) if (d !== "" && dirOf(d) === path) subdirs.push(d);
  subdirs.sort();
  const subdirSet = new Set(subdirs);

  const directFiles: string[] = [];
  for (const [id, p] of index.filePaths) if (dirOf(p) === path) directFiles.push(id);
  directFiles.sort((a, b) => index.filePaths.get(a)!.localeCompare(index.filePaths.get(b)!));

  // Map a file id to its level item (direct file id or sub-folder aggregate id).
  const fileItemOf = (fileId: string): string | null => {
    const p = index.filePaths.get(fileId);
    if (p === undefined) return null;
    if (dirOf(p) === path) return fileId;
    const prefix = path === "" ? "" : path + "/";
    if (path !== "" && !p.startsWith(prefix)) return null;
    const rest = path === "" ? p : p.slice(prefix.length);
    const sub = (path === "" ? "" : path + "/") + rest.split("/")[0];
    return subdirSet.has(sub) ? folderItemId(sub) : null;
  };

  const filesByItem = new Map<string, string[]>();
  for (const sub of subdirs) {
    const arr: string[] = [];
    for (const [fid, p] of index.filePaths) if (p === sub || p.startsWith(sub + "/")) arr.push(fid);
    filesByItem.set(folderItemId(sub), arr);
  }
  for (const fid of directFiles) filesByItem.set(fid, [fid]);

  // Aggregate edges from internal file→file, plus file→module / file→endpoint.
  const seen = new Set<string>();
  const edges: Edge[] = [];
  const modulesUsed = new Set<string>();
  const endpointsHere = new Set<string>();
  for (const e of index.edges) {
    const sItem = fileItemOf(e.source);
    if (!sItem) continue;
    const tKind = index.nodeKind.get(e.target);
    let tItem: string | null;
    let color = "#3f3f46";
    if (tKind === "module") {
      tItem = e.target;
      modulesUsed.add(e.target);
      color = "#3f3f46";
    } else if (tKind === "endpoint") {
      tItem = e.target;
      endpointsHere.add(e.target);
      color = "#0e7490";
    } else {
      tItem = fileItemOf(e.target);
    }
    if (!tItem || sItem === tItem) continue;
    const key = `${sItem}>${tItem}`;
    if (seen.has(key)) continue;
    seen.add(key);
    edges.push(baseEdge(`lvl:${key}`, sItem, tItem, color, tKind === "module"));
  }

  // --- positions: folders+files in a centre grid, modules left, endpoints right
  const centre: { id: string; node: Node }[] = [];
  for (const sub of subdirs) centre.push({ id: folderItemId(sub), node: makeNode(index, folderItemId(sub)) });
  for (const fid of directFiles) centre.push({ id: fid, node: makeNode(index, fid) });

  const cols = Math.min(MAX_COLS, Math.max(1, Math.ceil(Math.sqrt(Math.max(1, centre.length)))));
  const nodes: Node[] = [];
  centre.forEach((it, i) => {
    it.node.position = { x: (i % cols) * CELL_W, y: Math.floor(i / cols) * CELL_H };
    nodes.push(it.node);
  });

  const mods = [...modulesUsed].sort().slice(0, MODULE_CAP);
  mods.forEach((mid, i) => {
    const n = makeNode(index, mid);
    n.position = { x: -SIDE_GAP - 220, y: i * SIDE_ROW };
    nodes.push(n);
  });
  const eps = [...endpointsHere].sort();
  const rightX = cols * CELL_W + SIDE_GAP;
  eps.forEach((eid, i) => {
    const n = makeNode(index, eid);
    n.position = { x: rightX, y: i * SIDE_ROW };
    nodes.push(n);
  });

  return {
    nodes,
    edges,
    filesByItem,
    subdirCount: subdirs.length,
    fileCount: directFiles.length,
    moduleCount: mods.length,
    endpointCount: eps.length,
  };
}

const FLOW_COL = 420;
const FLOW_ROW = 132;

/** Egocentric flow for one item: importers (left) → item (centre) → dependencies (right). */
export function flowView(index: RepoIndex, focalId: string): { nodes: Node[]; edges: Edge[]; callers: number; deps: number } {
  let callers: string[];
  let deps: string[];

  if (focalId.startsWith("agg:")) {
    const folder = folderPathFromId(focalId);
    const inFolder = (id: string): boolean => {
      const p = index.filePaths.get(id);
      return p !== undefined && (p === folder || p.startsWith(folder + "/"));
    };
    const callerSet = new Set<string>();
    const depSet = new Set<string>();
    for (const e of index.edges) {
      const sIn = inFolder(e.source);
      const tIn = inFolder(e.target);
      if (sIn && !tIn) depSet.add(e.target);
      else if (!sIn && tIn) callerSet.add(e.source);
    }
    callers = [...callerSet];
    deps = [...depSet];
  } else {
    callers = [...new Set(index.inAdj.get(focalId) ?? [])];
    deps = [...new Set(index.outAdj.get(focalId) ?? [])];
  }
  // A bidirectional neighbor counts as a dependency (right side) only.
  const depSet = new Set(deps);
  callers = callers.filter((c) => !depSet.has(c));

  const labelOf = (id: string): string => {
    const p = index.filePaths.get(id);
    return p ? p : String((index.moduleData.get(id) ?? index.endpointData.get(id))?.label ?? id);
  };
  callers.sort((a, b) => labelOf(a).localeCompare(labelOf(b)));
  deps.sort((a, b) => labelOf(a).localeCompare(labelOf(b)));

  const nodes: Node[] = [];
  const focal = makeNode(index, focalId);
  focal.position = { x: 0, y: 0 };
  (focal.data as Record<string, unknown>).flowFocal = true;
  nodes.push(focal);

  const place = (ids: string[], x: number) => {
    ids.forEach((id, i) => {
      const n = makeNode(index, id);
      n.position = { x, y: (i - (ids.length - 1) / 2) * FLOW_ROW };
      nodes.push(n);
    });
  };
  place(callers, -FLOW_COL);
  place(deps, FLOW_COL);

  const edges: Edge[] = [];
  for (const c of callers) edges.push({ ...baseEdge(`flow:${c}>${focalId}`, c, focalId, "#22d3ee"), animated: true });
  for (const d of deps) {
    const color = index.nodeKind.get(d) === "module" ? "#6366f1" : "#34d399";
    edges.push({ ...baseEdge(`flow:${focalId}>${d}`, focalId, d, color, index.nodeKind.get(d) === "module"), animated: true });
  }

  return { nodes, edges, callers: callers.length, deps: deps.length };
}

/** Friendly name for a node id (for the flow toolbar). */
export function labelForId(index: RepoIndex, id: string): string {
  if (id.startsWith("agg:")) return baseOf(folderPathFromId(id)) || "root";
  const p = index.filePaths.get(id);
  if (p) return baseOf(p);
  return String((index.moduleData.get(id) ?? index.endpointData.get(id))?.label ?? id);
}

/** Breadcrumb segments for a path: [{ name, path }] from root → here. */
export function breadcrumb(path: string): { name: string; path: string }[] {
  const out = [{ name: "root", path: "" }];
  if (!path) return out;
  const segs = path.split("/");
  let acc = "";
  for (const s of segs) {
    acc = acc ? `${acc}/${s}` : s;
    out.push({ name: s, path: acc });
  }
  return out;
}

/** Deepest directory that contains every given file path. "" if none/spanning root. */
export function commonAncestor(filePaths: string[]): string {
  if (filePaths.length === 0) return "";
  const segsOf = (d: string): string[] => (d === "" ? [] : d.split("/"));
  let prefix = segsOf(dirOf(filePaths[0]));
  for (let i = 1; i < filePaths.length; i++) {
    const segs = segsOf(dirOf(filePaths[i]));
    let k = 0;
    while (k < prefix.length && k < segs.length && prefix[k] === segs[k]) k++;
    prefix = prefix.slice(0, k);
    if (prefix.length === 0) break;
  }
  return prefix.join("/");
}
