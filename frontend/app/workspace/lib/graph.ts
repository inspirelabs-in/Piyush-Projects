import { MarkerType, type Edge, type Node } from "@xyflow/react";
import type { GraphData } from "./api";

// extensionColor maps a file extension to a neon-tinted accent used by the file
// node styling. Color-coding by extension makes the topology readable at a glance.
export function extensionColor(pathOrLabel: string): string {
  const ext = pathOrLabel.toLowerCase().split(".").pop() ?? "";
  switch (ext) {
    case "ts":
      return "#3b82f6"; // blue
    case "tsx":
      return "#22d3ee"; // cyan
    case "js":
    case "mjs":
    case "cjs":
      return "#eab308"; // amber
    case "jsx":
      return "#f59e0b";
    case "sql":
    case "prisma":
      return "#a855f7"; // violet (db)
    case "json":
      return "#10b981";
    case "go":
      return "#00add8"; // Go gopher cyan
    case "rs":
      return "#f74c00"; // Rust orange
    default:
      return "#94a3b8"; // slate
  }
}

function isDatabaseFile(path: string): boolean {
  return /\.(sql|prisma)$/i.test(path);
}

// nodeTypeForKind maps the backend's semantic node kind to a React Flow custom
// node type.
function nodeTypeForKind(kind: string, path: string): string {
  switch (kind) {
    case "file":
      return isDatabaseFile(path) ? "synapseDatabase" : "synapseFile";
    case "module":
      return "synapseModule";
    case "endpoint":
      return "synapseEndpoint";
    default:
      return "synapseFile";
  }
}

// --- Nested folder layout ---------------------------------------------------
// Files are packed into their folder boxes, folders nest inside parent folders,
// and external modules / endpoints sit in columns to the right. Layout is a
// simple row-wrap (flow) packer applied recursively bottom-up so each folder is
// sized to fit its children.

const FILE_W = 184;
const FILE_H = 56;
const GAP = 20;
const PAD = 18;
const HEADER = 30; // folder title band height
const INNER_MAX_W = 3 * (FILE_W + GAP); // wrap target inside a folder
const TOP_MAX_W = 4 * (FILE_W + GAP); // wrap target for top-level folders
const SIDE_GAP = 160; // gap between the folder tree and the module/endpoint columns
const SIDE_COL = 300;
const SIDE_ROW = FILE_H + GAP;

interface Box {
  w: number;
  h: number;
}

interface FolderLayout {
  positions: { x: number; y: number }[]; // per child item, relative to content origin
  w: number;
  h: number;
}

interface FolderTree {
  id: string; // "folder:<relpath>"
  name: string;
  children: Map<string, FolderTree>;
  files: string[]; // file node ids (in declaration order)
  layout?: FolderLayout;
}

/** Returns the ancestor folder ids for a file path, e.g. "a/b/c.ts" -> ["folder:a","folder:a/b"]. */
export function ancestorFolderIds(filePath: string): string[] {
  const segs = filePath.split("/").filter(Boolean).slice(0, -1);
  const out: string[] = [];
  let acc = "";
  for (const dir of segs) {
    acc = acc ? `${acc}/${dir}` : dir;
    out.push(`folder:${acc}`);
  }
  return out;
}

function buildTree(files: GraphData["nodes"]): FolderTree {
  const root: FolderTree = { id: "folder:", name: "", children: new Map(), files: [] };
  for (const f of files) {
    const path = String(f.data.path ?? f.data.label ?? "");
    const segs = path.split("/").filter(Boolean);
    const dirs = segs.slice(0, -1);
    let cur = root;
    let acc = "";
    for (const dir of dirs) {
      acc = acc ? `${acc}/${dir}` : dir;
      const id = `folder:${acc}`;
      let next = cur.children.get(id);
      if (!next) {
        next = { id, name: dir, children: new Map(), files: [] };
        cur.children.set(id, next);
      }
      cur = next;
    }
    cur.files.push(f.id);
  }
  return root;
}

// packRows lays boxes left-to-right, wrapping to a new row when the next box
// would exceed maxW. Returns each box's position and the packed bounds.
function packRows(boxes: Box[], maxW: number): { pos: { x: number; y: number }[]; w: number; h: number } {
  const pos: { x: number; y: number }[] = [];
  let x = 0;
  let y = 0;
  let rowH = 0;
  let maxRowW = 0;
  for (const b of boxes) {
    if (x > 0 && x + b.w > maxW) {
      y += rowH + GAP;
      x = 0;
      rowH = 0;
    }
    pos.push({ x, y });
    x += b.w + GAP;
    rowH = Math.max(rowH, b.h);
    maxRowW = Math.max(maxRowW, x - GAP);
  }
  return { pos, w: maxRowW, h: y + rowH };
}

// measure sizes a folder (and its subtree) and caches the child layout.
function measure(folder: FolderTree): FolderLayout {
  if (folder.layout) return folder.layout;
  const boxes: Box[] = [];
  for (const cf of folder.children.values()) {
    const s = measure(cf);
    boxes.push({ w: s.w, h: s.h });
  }
  for (let i = 0; i < folder.files.length; i++) {
    boxes.push({ w: FILE_W, h: FILE_H });
  }
  const packed = packRows(boxes, INNER_MAX_W);
  const innerW = Math.max(packed.w, FILE_W);
  const innerH = Math.max(packed.h, FILE_H);
  const layout: FolderLayout = {
    positions: packed.pos,
    w: innerW + PAD * 2,
    h: innerH + HEADER + PAD,
  };
  folder.layout = layout;
  return layout;
}

// toFlow converts the backend graph payload into React Flow nodes + edges:
// nested folder group nodes containing file nodes, plus module/endpoint columns.
export function toFlow(graph: GraphData): { nodes: Node[]; edges: Edge[] } {
  const files = graph.nodes.filter((n) => String(n.data.kind) === "file");
  const modules = graph.nodes.filter((n) => String(n.data.kind) === "module");
  const endpoints = graph.nodes.filter((n) => String(n.data.kind) === "endpoint");
  const fileById = new Map(files.map((f) => [f.id, f]));

  const out: Node[] = [];

  const fileNodeFor = (fid: string, parentId: string | null, x: number, y: number): Node => {
    const orig = fileById.get(fid)!;
    const path = String(orig.data.path ?? orig.data.label ?? "");
    const node: Node = {
      id: fid,
      type: nodeTypeForKind(String(orig.data.kind), path),
      position: { x, y },
      data: { ...orig.data, accent: extensionColor(path) },
    };
    if (parentId) node.parentId = parentId;
    return node;
  };

  // Emit a folder node (parent-before-children for React Flow), then its items.
  const emit = (folder: FolderTree, parentId: string | null, x: number, y: number) => {
    const L = measure(folder);
    const folderNode: Node = {
      id: folder.id,
      type: "synapseFolder",
      position: { x, y },
      data: { label: folder.name || "repo", kind: "folder" },
      style: { width: L.w, height: L.h },
    };
    if (parentId) folderNode.parentId = parentId;
    out.push(folderNode);

    const childFolders = [...folder.children.values()];
    let idx = 0;
    for (const cf of childFolders) {
      const p = L.positions[idx++];
      emit(cf, folder.id, PAD + p.x, HEADER + p.y);
    }
    for (const fid of folder.files) {
      const p = L.positions[idx++];
      out.push(fileNodeFor(fid, folder.id, PAD + p.x, HEADER + p.y));
    }
  };

  // Top level: place top-level folders + root-level files in a wide flow (no
  // enclosing repo box — keeps the canvas from being one giant container).
  const root = buildTree(files);
  const topFolders = [...root.children.values()];
  topFolders.forEach(measure);
  const topBoxes: Box[] = [
    ...topFolders.map((f) => ({ w: f.layout!.w, h: f.layout!.h })),
    ...root.files.map(() => ({ w: FILE_W, h: FILE_H })),
  ];
  const topPacked = packRows(topBoxes, TOP_MAX_W);

  let ti = 0;
  for (const cf of topFolders) {
    const p = topPacked.pos[ti++];
    emit(cf, null, p.x, p.y);
  }
  for (const fid of root.files) {
    const p = topPacked.pos[ti++];
    out.push(fileNodeFor(fid, null, p.x, p.y));
  }

  // Module + endpoint columns to the right of the folder tree.
  const xMod = topPacked.w + SIDE_GAP;
  const xEnd = xMod + SIDE_COL;
  modules.forEach((n, i) => {
    out.push({ id: n.id, type: "synapseModule", position: { x: xMod, y: i * SIDE_ROW }, data: { ...n.data } });
  });
  endpoints.forEach((n, i) => {
    out.push({ id: n.id, type: "synapseEndpoint", position: { x: xEnd, y: i * SIDE_ROW }, data: { ...n.data } });
  });

  const edges: Edge[] = graph.edges.map((e) => ({
    id: e.id,
    source: e.source,
    target: e.target,
    type: "smoothstep",
    label: e.label,
    animated: e.animated,
    markerEnd: { type: MarkerType.ArrowClosed, width: 16, height: 16, color: "#52525b" },
    style: { stroke: "#3f3f46", strokeWidth: 1.5 },
    labelStyle: { fill: "#a1a1aa", fontSize: 10, fontFamily: "var(--font-mono)" },
    labelBgStyle: { fill: "#111113" },
  }));

  return { nodes: out, edges };
}
