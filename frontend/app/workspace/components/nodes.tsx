"use client";

import { Handle, Position, useStore, type NodeProps, type NodeTypes } from "@xyflow/react";

import { useCanvasActions } from "./CanvasActions";

// Semantic zoom: collapse the continuous viewport zoom into a discrete detail
// bucket. Selecting THIS (not the raw zoom) means a node re-renders only when it
// crosses a threshold, never on every wheel tick. macro (<0.5) = name only;
// mid (0.5–0.8) = + language; micro (>0.8) = full detail.
type Detail = "macro" | "mid" | "micro";
function useDetail(): Detail {
  return useStore((s) => {
    const z = s.transform[2];
    return z < 0.5 ? "macro" : z <= 0.8 ? "mid" : "micro";
  });
}

// Shared handle styling — small, unobtrusive connection points.
function Ports() {
  return (
    <>
      <Handle
        type="target"
        position={Position.Left}
        className="!h-1.5 !w-1.5 !border-0 !bg-zinc-600"
      />
      <Handle
        type="source"
        position={Position.Right}
        className="!h-1.5 !w-1.5 !border-0 !bg-zinc-600"
      />
    </>
  );
}

// FlowButton — the "focus flow" affordance present on every node. Clicking it
// isolates that item's cross-repo connections (importers + dependencies).
function FlowButton({ id, className = "" }: { id: string; className?: string }) {
  const { focusFlow } = useCanvasActions();
  return (
    <button
      className={
        "nodrag grid h-4 w-4 shrink-0 place-items-center rounded text-[11px] leading-none text-neutral-500 transition-colors hover:bg-white/10 hover:text-accent " +
        className
      }
      onClick={(e) => {
        e.stopPropagation();
        focusFlow(id);
      }}
      title="Focus flow — isolate this item's connections"
    >
      ⊙
    </button>
  );
}

interface FolderData {
  label: string;
  collapsed?: boolean; // set by the canvas when this folder is collapsed
  count?: number; // hidden-descendant count, shown while collapsed
}

// FolderNode — a plain directory container (legacy nested view; the drill-down
// view uses DrillFolderNode instead).
function FolderNode({ data }: NodeProps) {
  const d = data as unknown as FolderData;
  return (
    <div className="synapse-folder h-full w-full rounded-xl border border-panel-border/70 bg-neutral-900/25">
      <div className="flex items-center gap-1.5 px-2.5 py-1.5 text-[11px] font-medium text-neutral-400">
        <span className="text-[10px]">📁</span>
        <span className="truncate font-mono">{d.label}</span>
      </div>
    </div>
  );
}

interface FileData {
  label: string;
  path?: string;
  language?: string;
  exports?: number;
  accent?: string;
}

// FileNode — a code file, color-coded by extension via a left accent rail.
// Semantic zoom: the metadata subline is dropped as the viewport zooms out so a
// dense graph reads cleanly from afar (macro = name only). Full detail at the
// default zoom is unchanged.
function FileNode({ id, data }: NodeProps) {
  const d = data as unknown as FileData;
  const detail = useDetail();
  const accent = d.accent ?? "#94a3b8";
  return (
    <div className="synapse-card relative flex w-[200px] overflow-hidden rounded-lg border border-panel-border bg-panel">
      <span className="w-1 shrink-0" style={{ backgroundColor: accent }} />
      <div className="min-w-0 flex-1 px-3 py-2">
        <div className="truncate pr-3 font-mono text-[12px] font-medium text-neutral-100">
          {d.label}
        </div>
        {detail !== "macro" && (
          <div className="mt-0.5 flex items-center gap-2 text-[10px] text-neutral-500">
            <span style={{ color: accent }}>{d.language ?? "file"}</span>
            {detail === "micro" && typeof d.exports === "number" && d.exports > 0 && (
              <span>· {d.exports} exports</span>
            )}
          </div>
        )}
      </div>
      <FlowButton id={id} className="absolute right-1 top-1 z-10" />
      <Ports />
    </div>
  );
}

interface ModuleData {
  label: string;
}

// ModuleNode — an external npm/Node dependency, rendered as a dim pill.
function ModuleNode({ id, data }: NodeProps) {
  const d = data as unknown as ModuleData;
  return (
    <div className="synapse-card flex items-center gap-1.5 rounded-full border border-dashed border-zinc-700 bg-neutral-900 py-1.5 pl-3 pr-2">
      <span className="text-[11px]">📦</span>
      <span className="font-mono text-[11px] text-neutral-400">{d.label}</span>
      <FlowButton id={id} />
      <Ports />
    </div>
  );
}

interface EndpointData {
  label: string;
  method?: string;
  path?: string;
}

const methodColors: Record<string, string> = {
  GET: "#22d3ee",
  POST: "#34d399",
  PUT: "#fbbf24",
  PATCH: "#fbbf24",
  DELETE: "#f87171",
};

// EndpointNode — an HTTP route, rendered as a distinct rounded tag with a
// method badge.
function EndpointNode({ id, data }: NodeProps) {
  const d = data as unknown as EndpointData;
  const color = methodColors[d.method ?? ""] ?? "#a78bfa";
  return (
    <div className="synapse-card flex items-center gap-2 rounded-full border bg-neutral-900 py-1.5 pl-1.5 pr-2"
      style={{ borderColor: color }}>
      <span
        className="rounded-full px-2 py-0.5 font-mono text-[10px] font-bold text-neutral-950"
        style={{ backgroundColor: color }}
      >
        {d.method ?? "API"}
      </span>
      <span className="font-mono text-[11px] text-neutral-200">
        {d.path ?? d.label}
      </span>
      <FlowButton id={id} />
      <Ports />
    </div>
  );
}

interface DatabaseData {
  label: string;
  accent?: string;
}

// DatabaseNode — a schema/table file, rendered as a stacked "database block".
function DatabaseNode({ data }: NodeProps) {
  const d = data as unknown as DatabaseData;
  const accent = d.accent ?? "#a855f7";
  return (
    <div className="synapse-card relative w-[180px] rounded-md border bg-neutral-900"
      style={{ borderColor: accent }}>
      <span
        className="absolute -top-1 left-2 right-2 h-1 rounded-t-md opacity-60"
        style={{ backgroundColor: accent }}
      />
      <div className="flex items-center gap-2 px-3 py-2.5">
        <span className="text-[12px]">🗄️</span>
        <span className="truncate font-mono text-[12px] text-neutral-100">
          {d.label}
        </span>
      </div>
      <Ports />
    </div>
  );
}

interface FunctionNodeData {
  symbol: string;
  chunkType?: string;
  code: string;
  lines?: string;
  expanded?: boolean; // open state is owned by the canvas so it can reflow the column
}

// FunctionNode — a symbol-level node (function/class/...) attached to its file.
// Collapsed to a header row; click to reveal the source code inline. The open
// state lives in the canvas (via toggleFn) so the layout can push neighbours down
// and keep expanded cards from overlapping.
function FunctionNode({ id, data }: NodeProps) {
  const d = data as unknown as FunctionNodeData;
  const { toggleFn } = useCanvasActions();
  const open = !!d.expanded;
  return (
    <div
      className="synapse-card w-fit min-w-[210px] overflow-hidden rounded-lg border border-indigo-500/60 bg-neutral-950"
      style={open ? { zIndex: 10 } : undefined}
    >
      <button
        onClick={(e) => {
          e.stopPropagation();
          toggleFn(id);
        }}
        className="flex w-full items-center gap-1.5 px-2.5 py-1.5 text-left"
      >
        <span className="font-mono text-[11px] text-indigo-300">ƒ</span>
        <span className="min-w-0 flex-1 truncate font-mono text-[11px] text-neutral-100">
          {d.symbol}
        </span>
        {d.lines && <span className="shrink-0 text-[9px] text-neutral-600">{d.lines}</span>}
        <span className="shrink-0 select-none text-[9px] text-neutral-500">{open ? "▾" : "▸"}</span>
      </button>
      {open && (
        <pre className="max-h-56 w-[360px] overflow-auto border-t border-panel-border bg-black px-2.5 py-1.5 font-mono text-[10px] leading-relaxed text-neutral-200">
          <code>{d.code}</code>
        </pre>
      )}
      <Ports />
    </div>
  );
}

interface GapData {
  label: string;
  kind?: string;
  suggested?: string;
}

// GapNode — a speculative "net-new" capability (Red) the blueprint says must be
// built. Rendered as a glowing, dashed, pulsing box to read as not-yet-existing.
function GapNode({ data }: NodeProps) {
  const d = data as unknown as GapData;
  return (
    <div className="synapse-gap-card w-[180px] rounded-lg px-3 py-2.5">
      <div className="flex items-center gap-1.5">
        <span className="text-[11px]">✨</span>
        <span className="truncate font-mono text-[12px] font-medium text-red-200">
          {d.label}
        </span>
      </div>
      <div className="mt-0.5 text-[9px] uppercase tracking-widest text-red-400/80">
        new {d.kind ?? "capability"}
      </div>
      {d.suggested && (
        <div className="mt-1 truncate font-mono text-[10px] text-neutral-500">
          {d.suggested}
        </div>
      )}
      <Ports />
    </div>
  );
}

interface DrillFolderData {
  name: string;
  path: string;
  fileCount: number;
  subdirCount: number;
  blueprint?: "green" | "yellow"; // reuse tint in blueprint mode
}

// DrillFolderNode — a folder shown as an aggregate card in the staged drill-down
// view. Clicking it drills into that folder (handled by the canvas). It collapses
// an entire subtree into one legible card so large codebases stay navigable.
function DrillFolderNode({ id, data }: NodeProps) {
  const d = data as unknown as DrillFolderData;
  return (
    <div className="synapse-card group/df relative w-[210px] cursor-pointer overflow-hidden rounded-xl border border-panel-border bg-panel transition-colors hover:border-accent">
      <FlowButton id={id} className="absolute right-1 top-1 z-10" />
      <div className="flex items-center gap-2 border-b border-panel-border px-3 py-2.5" style={{ background: "rgba(99,102,241,0.07)" }}>
        <span className="grid h-7 w-7 shrink-0 place-items-center rounded-md bg-accent/15 text-[14px]">📁</span>
        <span className="truncate pr-4 font-mono text-[13px] font-semibold text-neutral-100">{d.name}</span>
      </div>
      <div className="flex items-center justify-between px-3 py-2">
        <span className="text-[10px] text-neutral-500">
          {d.fileCount} file{d.fileCount === 1 ? "" : "s"}
          {d.subdirCount > 0 ? ` · ${d.subdirCount} folder${d.subdirCount === 1 ? "" : "s"}` : ""}
        </span>
        <span className="flex items-center gap-0.5 text-[10px] font-medium text-accent opacity-0 transition-opacity group-hover/df:opacity-100">
          open <span aria-hidden>›</span>
        </span>
      </div>
      <Ports />
    </div>
  );
}

// Stable, module-level node type registry — React Flow requires a stable
// reference here (a new object each render forces full remounts).
export const nodeTypes: NodeTypes = {
  synapseFolder: FolderNode,
  synapseFile: FileNode,
  synapseFunction: FunctionNode,
  synapseModule: ModuleNode,
  synapseEndpoint: EndpointNode,
  synapseDatabase: DatabaseNode,
  synapseGap: GapNode,
  drillFolder: DrillFolderNode,
};
