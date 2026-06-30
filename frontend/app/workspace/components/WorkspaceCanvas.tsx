"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Background,
  BackgroundVariant,
  Controls,
  MiniMap,
  MarkerType,
  Panel,
  ReactFlow,
  ReactFlowProvider,
  getViewportForBounds,
  useReactFlow,
  useStore,
  type Edge,
  type Node,
} from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import { gsap } from "gsap";

import type { BlueprintResponse, CallEdge, FunctionHit, GraphData } from "../lib/api";
import { fileNodeId } from "../lib/api";
import {
  breadcrumb,
  buildIndex,
  commonAncestor,
  flowView,
  folderPathFromId,
  labelForId,
  levelView,
  type LevelView,
} from "../lib/levelGraph";
import { nodeTypes } from "./nodes";
import { CanvasActionsContext } from "./CanvasActions";

interface WorkspaceCanvasProps {
  graph: GraphData | null;
  highlightedFiles: string[];
  focusNonce: number; // bump to navigate/highlight (chat answers, tour steps, function clicks)
  onSelectNode: (id: string | null, label: string | null) => void;
  blueprint: BlueprintResponse | null;
  functions: FunctionHit[]; // symbol-level nodes for the file-detail level
  callEdges?: CallEdge[]; // intra-file caller→callee edges for the file-detail level
  onExpandFile?: (path: string) => void; // fetch a file's functions when drilled into
  perspective?: "synaptic" | "executive"; // executive masks files, showing folders only
}

// layerOf assigns each function node a column by call depth (callees sit to the
// right of their callers), so the file's call flow reads left→right as a pipeline.
// Cycles are broken by capping the relaxation passes at the node count.
function layerOf(ids: string[], edges: Edge[]): Map<string, number> {
  const layer = new Map<string, number>();
  ids.forEach((id) => layer.set(id, 0));
  for (let iter = 0; iter < ids.length; iter++) {
    let changed = false;
    for (const e of edges) {
      const ls = layer.get(e.source) ?? 0;
      if ((layer.get(e.target) ?? 0) < ls + 1) {
        layer.set(e.target, ls + 1);
        changed = true;
      }
    }
    if (!changed) break;
  }
  return layer;
}

// Function-card geometry. We stack each column by REAL card height so an expanded
// code panel pushes the cards below it down instead of overlapping them.
const FN_COL_W = 400; // ≥ expanded code width (360) + breathing room
const FN_GAP = 22;
const FN_COLLAPSED_H = 46;
const FN_EXPANDED_H = 286; // header + max-h-56 code panel

// stackColumns lays out function nodes: each id is assigned a column, then columns
// are packed top-to-bottom advancing by each card's measured height.
function stackColumns(
  ids: string[],
  columnOf: Map<string, number>,
  isExpanded: (id: string) => boolean,
): Map<string, { x: number; y: number }> {
  const byCol = new Map<number, string[]>();
  for (const id of ids) {
    const c = columnOf.get(id) ?? 0;
    (byCol.get(c) ?? byCol.set(c, []).get(c)!).push(id);
  }
  const pos = new Map<string, { x: number; y: number }>();
  for (const [c, members] of byCol) {
    let y = 0;
    for (const id of members) {
      pos.set(id, { x: c * FN_COL_W, y });
      y += (isExpanded(id) ? FN_EXPANDED_H : FN_COLLAPSED_H) + FN_GAP;
    }
  }
  return pos;
}

const EMPTY_VIEW: LevelView = {
  nodes: [],
  edges: [],
  filesByItem: new Map(),
  subdirCount: 0,
  fileCount: 0,
  moduleCount: 0,
  endpointCount: 0,
};

export default function WorkspaceCanvas(props: WorkspaceCanvasProps) {
  return (
    <ReactFlowProvider>
      <Flow {...props} />
    </ReactFlowProvider>
  );
}

function Flow({
  graph,
  highlightedFiles,
  focusNonce,
  onSelectNode,
  blueprint,
  functions,
  callEdges = [],
  onExpandFile,
  perspective = "synaptic",
}: WorkspaceCanvasProps) {
  const index = useMemo(() => (graph ? buildIndex(graph) : null), [graph]);
  const filePathSet = useMemo(() => new Set(index ? index.filePaths.values() : []), [index]);

  // Current drill location: "" = root, "src/utils" = a folder, "src/utils/x.ts" = a file.
  const [path, setPath] = useState("");
  const [highlightIds, setHighlightIds] = useState<Set<string>>(new Set());
  // Focus-flow: when set, the canvas shows this node's cross-repo connections.
  const [flowFocus, setFlowFocus] = useState<string | null>(null);
  // Full path of the file node currently hovered in flow mode (screen-space tip).
  const [hoverPath, setHoverPath] = useState<string | null>(null);
  // Which function cards are expanded (open code panel) in the file-detail level.
  const [expandedFns, setExpandedFns] = useState<Set<string>>(new Set());

  const focusFlow = useCallback((id: string) => {
    setFlowFocus(id);
    setHighlightIds(new Set());
  }, []);
  const toggleFn = useCallback((id: string) => {
    setExpandedFns((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);
  const canvasActions = useMemo(() => ({ focusFlow, toggleFn }), [focusFlow, toggleFn]);

  const reactFlow = useReactFlow();
  const paneWidth = useStore((s) => s.width);
  const paneHeight = useStore((s) => s.height);
  const wrapperRef = useRef<HTMLDivElement>(null);

  const isFileLevel = filePathSet.has(path);
  const crumbs = useMemo(() => breadcrumb(path), [path]);

  // The folder level (subfolders + files + aggregated edges).
  const folderLevel = useMemo(
    () => (index && !isFileLevel ? levelView(index, path) : EMPTY_VIEW),
    [index, path, isFileLevel],
  );

  // The file-detail level: the file's functions, wired by their intra-file call
  // graph (caller → callee). With call edges present we lay them out left→right by
  // call depth; otherwise we fall back to a tidy grid.
  const fileLevel = useMemo(() => {
    if (!isFileLevel) return { nodes: [] as Node[], edges: [] as Edge[] };

    // One node per logical symbol: the chunker splits long bodies into "foo#part2"
    // segments, so collapse them onto the first part (it carries the signature) and
    // show the clean base name.
    const baseOf = (sym: string) => sym.split("#")[0];
    const seenBase = new Set<string>();
    const fns = functions
      .filter((f) => f.file === path)
      .filter((f) => {
        const b = baseOf(f.symbol);
        if (seenBase.has(b)) return false;
        seenBase.add(b);
        return true;
      })
      .map((f) => ({ ...f, symbol: baseOf(f.symbol) }));
    const nodeId = (fn: FunctionHit, i: number) => `fn:${fn.file}:${fn.symbol}:${i}`;
    const ids = fns.map(nodeId);

    // symbol → node id, dual-keyed by full symbol AND short name (after the last
    // dot) so "Store.Run" and "Run" both resolve regardless of which form a parser
    // emitted for the call's endpoint vs. the chunk's symbol name.
    const idBySymbol = new Map<string, string>();
    const addKey = (k: string | undefined, id: string) => {
      if (k && !idBySymbol.has(k)) idBySymbol.set(k, id);
    };
    fns.forEach((fn, i) => {
      const id = nodeId(fn, i);
      addKey(fn.symbol, id);
      addKey(fn.symbol.split(".").pop(), id);
    });
    const resolve = (name: string) => {
      const base = baseOf(name);
      return idBySymbol.get(base) ?? idBySymbol.get(base.split(".").pop() ?? base);
    };

    const seen = new Set<string>();
    const edges: Edge[] = [];
    for (const c of callEdges) {
      const source = resolve(c.caller);
      const target = resolve(c.callee);
      if (!source || !target || source === target) continue; // skip unresolved + self-calls
      const key = `${source}__${target}`;
      if (seen.has(key)) continue;
      seen.add(key);
      edges.push({
        id: `call:${key}`,
        source,
        target,
        type: "smoothstep",
        className: "rf-call-edge",
        markerEnd: { type: MarkerType.ArrowClosed, color: "#818cf8", width: 15, height: 15 },
        style: { stroke: "#818cf8", strokeWidth: 1.5 },
      });
    }

    // Column assignment: by call depth when the functions are wired together,
    // otherwise a square-ish grid spread across a few columns.
    const columnOf = new Map<string, number>();
    if (edges.length > 0) {
      const layer = layerOf(ids, edges);
      ids.forEach((id) => columnOf.set(id, layer.get(id) ?? 0));
    } else {
      const cols = Math.min(3, Math.max(1, Math.ceil(Math.sqrt(Math.max(1, ids.length)))));
      ids.forEach((id, i) => columnOf.set(id, i % cols));
    }
    const positions = stackColumns(ids, columnOf, (id) => expandedFns.has(id));

    const nodes: Node[] = fns.map((fn, i) => {
      const id = nodeId(fn, i);
      return {
        id,
        type: "synapseFunction",
        position: positions.get(id) ?? { x: 0, y: 0 },
        data: {
          symbol: fn.symbol,
          chunkType: fn.chunk_type,
          code: fn.code,
          lines: `${fn.start_line}–${fn.end_line}`,
          expanded: expandedFns.has(id),
        },
      };
    });
    return { nodes, edges };
  }, [isFileLevel, functions, path, callEdges, expandedFns]);

  // Focus-flow view (importers ← item → dependencies), independent of the level.
  const flow = useMemo(
    () => (index && flowFocus ? flowView(index, flowFocus) : null),
    [index, flowFocus],
  );

  const view: LevelView = flow
    ? { ...EMPTY_VIEW, nodes: flow.nodes, edges: flow.edges }
    : isFileLevel
      ? { ...EMPTY_VIEW, nodes: fileLevel.nodes, edges: fileLevel.edges }
      : folderLevel;

  // Reset to root when the repo changes.
  useEffect(() => {
    setPath("");
    setHighlightIds(new Set());
    setFlowFocus(null);
    setExpandedFns(new Set());
  }, [graph]);

  // Entering a file level (via click OR breadcrumb) loads its functions.
  useEffect(() => {
    if (isFileLevel) onExpandFile?.(path);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path, isFileLevel]);

  // Blueprint discovery → jump to the root capability overview.
  useEffect(() => {
    if (blueprint) {
      setPath("");
      setHighlightIds(new Set());
      setFlowFocus(null);
    }
  }, [blueprint]);

  // One-time fade-in.
  useEffect(() => {
    if (!wrapperRef.current) return;
    const ctx = gsap.context(() => {
      gsap.fromTo(wrapperRef.current, { opacity: 0 }, { opacity: 1, duration: 0.5, ease: "power2.out" });
    }, wrapperRef);
    return () => ctx.revert();
  }, []);

  // Frame the level (or flow) whenever it changes.
  useEffect(() => {
    if (view.nodes.length === 0) return;
    const t = setTimeout(() => reactFlow.fitView({ padding: 0.22, duration: 480, maxZoom: 1.4 }), 70);
    return () => clearTimeout(t);
  }, [path, graph, flowFocus, view.nodes.length, reactFlow]);

  // GSAP camera onto a set of node ids (absolute positions, so nested nodes work).
  const focusOn = useCallback(
    (ids: string[]) => {
      if (!ids.length || !paneWidth || !paneHeight) return;
      let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
      for (const id of ids) {
        const n = reactFlow.getInternalNode(id);
        if (!n) continue;
        const p = n.internals.positionAbsolute;
        const w = n.measured?.width ?? 210;
        const h = n.measured?.height ?? 90;
        minX = Math.min(minX, p.x);
        minY = Math.min(minY, p.y);
        maxX = Math.max(maxX, p.x + w);
        maxY = Math.max(maxY, p.y + h);
      }
      if (!Number.isFinite(minX)) return;
      const target = getViewportForBounds(
        { x: minX, y: minY, width: Math.max(1, maxX - minX), height: Math.max(1, maxY - minY) },
        paneWidth, paneHeight, 0.3, 1.5, 0.4,
      );
      const cur = reactFlow.getViewport();
      const tween = { x: cur.x, y: cur.y, zoom: cur.zoom };
      gsap.to(tween, {
        x: target.x, y: target.y, zoom: target.zoom, duration: 0.8, ease: "power3.inOut",
        onUpdate: () => reactFlow.setViewport({ x: tween.x, y: tween.y, zoom: tween.zoom }),
      });
    },
    [reactFlow, paneWidth, paneHeight],
  );

  // Chat answers / tour steps / function clicks (highlightedFiles + focusNonce):
  // drill to the deepest folder containing the files and highlight the items there.
  useEffect(() => {
    if (!index || highlightedFiles.length === 0 || blueprint) return;
    const anc = commonAncestor(highlightedFiles);
    const lv = levelView(index, anc);
    const wanted = new Set(highlightedFiles.map(fileNodeId));
    const items = new Set<string>();
    for (const [item, files] of lv.filesByItem) {
      if (files.some((f) => wanted.has(f))) items.add(item);
    }
    setFlowFocus(null);
    setPath(anc);
    setHighlightIds(items);
    const t = setTimeout(() => focusOn(items.size ? [...items] : lv.nodes.map((n) => n.id)), 110);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [focusNonce]);

  // Blueprint speculative gap nodes (shown beside the root overview).
  const { gapNodes, gapEdges } = useMemo(() => {
    if (!blueprint || blueprint.gaps.length === 0) return { gapNodes: [] as Node[], gapEdges: [] as Edge[] };
    let maxX = 0;
    for (const n of view.nodes) maxX = Math.max(maxX, n.position.x);
    const gx = maxX + 340;
    const gn: Node[] = blueprint.gaps.map((g, i) => ({
      id: g.id,
      type: "synapseGap",
      position: { x: gx, y: i * 120 },
      data: { label: g.label, kind: g.kind, suggested: g.suggested_file },
    }));
    const present = new Set([...view.nodes.map((n) => n.id), ...gn.map((n) => n.id)]);
    const ge: Edge[] = blueprint.gap_edges
      .filter((e) => present.has(e.source) && present.has(e.target))
      .map((e, i) => ({
        id: `bp-${e.source}-${e.target}-${i}`,
        source: e.source,
        target: e.target,
        type: "smoothstep",
        animated: true,
        className: "bp-gap-edge",
        style: { stroke: "#f87171", strokeWidth: 1.5, strokeDasharray: "6 4" },
      }));
    return { gapNodes: gn, gapEdges: ge };
  }, [blueprint, view.nodes]);

  const navigate = useCallback((to: string) => {
    setPath(to);
    setHighlightIds(new Set());
    setFlowFocus(null);
    setExpandedFns(new Set()); // a fresh file/folder starts with all cards collapsed
  }, []);

  const onNodeClick = useCallback(
    (_: React.MouseEvent, node: Node) => {
      if (node.id.startsWith("fn:")) return; // function node toggles its own code
      if (blueprint) return; // blueprint overview is read-only

      // In flow mode, clicking a connected node re-centers the flow on it.
      if (flowFocus) {
        if (node.id !== flowFocus) {
          setFlowFocus(node.id);
          onSelectNode(node.id, labelForId(index!, node.id));
        }
        return;
      }

      if (node.type === "drillFolder") {
        const p = folderPathFromId(node.id);
        navigate(p);
        onSelectNode(node.id, String((node.data as { name?: string }).name ?? p));
        return;
      }

      // A file node: drill into its detail (functions). Modules/endpoints have no
      // drill target, so a body click just selects them (use ⊙ for their flow).
      const filePath = String((node.data as { path?: string }).path ?? "");
      onSelectNode(node.id, String((node.data as { label?: string }).label ?? node.id));
      if (filePath && filePathSet.has(filePath)) {
        navigate(filePath);
      }
    },
    [blueprint, flowFocus, index, navigate, onSelectNode, filePathSet],
  );

  const onPaneClick = useCallback(() => {
    if (blueprint) return;
    setHighlightIds(new Set());
    setFlowFocus(null);
    onSelectNode(null, null);
  }, [blueprint, onSelectNode]);

  // Flow mode: reveal the full path of a hovered file node (modules/endpoints have
  // no path and are skipped, so only files surface a tooltip).
  const onNodeMouseEnter = useCallback(
    (_: React.MouseEvent, node: Node) => {
      if (!flowFocus) return;
      const p = (node.data as { path?: string }).path;
      setHoverPath(p && filePathSet.has(p) ? p : null);
    },
    [flowFocus, filePathSet],
  );
  const onNodeMouseLeave = useCallback(() => setHoverPath(null), []);

  // --- Compose the rendered nodes/edges -------------------------------------
  const displayNodes = useMemo(() => {
    const baseNodes = [...view.nodes];

    if (blueprint) {
      const green = new Set(blueprint.highlights.green);
      const yellow = new Set(blueprint.highlights.yellow);
      const colored = baseNodes.map((n) => {
        const files = view.filesByItem.get(n.id);
        let cls = "bp-dim";
        if (files && files.length) {
          if (files.some((f) => green.has(f))) cls = "bp-green";
          else if (files.some((f) => yellow.has(f))) cls = "bp-yellow";
        }
        return { ...n, className: cls };
      });
      return [...colored, ...gapNodes];
    }

    let result =
      highlightIds.size === 0
        ? baseNodes
        : baseNodes.map((n) => ({
            ...n,
            className: highlightIds.has(n.id) ? "rf-active" : "rf-dim",
          }));
    if (perspective === "executive") {
      result = result.filter((n) => n.type === "drillFolder");
    }
    return result;
  }, [view, highlightIds, blueprint, gapNodes, perspective]);

  const displayEdges = useMemo(() => {
    if (blueprint) {
      return [...view.edges.map((e) => ({ ...e, className: "rf-dim" })), ...gapEdges];
    }
    let result =
      highlightIds.size === 0
        ? view.edges
        : view.edges.map((e) => {
            const active = highlightIds.has(e.source) && highlightIds.has(e.target);
            return { ...e, className: active ? "rf-active" : "rf-dim", animated: active };
          });
    if (perspective === "executive") {
      const visible = new Set(view.nodes.filter((n) => n.type === "drillFolder").map((n) => n.id));
      result = result.filter((e) => visible.has(e.source) && visible.has(e.target));
    }
    return result;
  }, [view, highlightIds, blueprint, gapEdges, perspective]);

  const minimapColor = useCallback((n: Node) => {
    switch (n.type) {
      case "drillFolder":
        return "#6366f1";
      case "synapseFunction":
        return "#818cf8";
      case "synapseDatabase":
        return "#a855f7";
      case "synapseGap":
        return "#f87171";
      default:
        return "#3b82f6";
    }
  }, []);

  const levelLabel = isFileLevel
    ? `${view.nodes.length} function${view.nodes.length === 1 ? "" : "s"}` +
      (view.edges.length ? ` · ${view.edges.length} call${view.edges.length === 1 ? "" : "s"}` : "")
    : `${view.subdirCount} folder${view.subdirCount === 1 ? "" : "s"} · ${view.fileCount} file${view.fileCount === 1 ? "" : "s"}` +
      (view.moduleCount ? ` · ${view.moduleCount} module${view.moduleCount === 1 ? "" : "s"}` : "") +
      (view.endpointCount ? ` · ${view.endpointCount} route${view.endpointCount === 1 ? "" : "s"}` : "");

  const flowName = flowFocus && index ? labelForId(index, flowFocus) : null;

  return (
    <CanvasActionsContext.Provider value={canvasActions}>
      <div ref={wrapperRef} className="h-full w-full">
        <ReactFlow
          nodes={displayNodes}
          edges={displayEdges}
          onNodeClick={onNodeClick}
          onNodeMouseEnter={onNodeMouseEnter}
          onNodeMouseLeave={onNodeMouseLeave}
          onPaneClick={onPaneClick}
          nodeTypes={nodeTypes}
          colorMode="dark"
          nodesDraggable={false}
          fitView
          minZoom={0.3}
          maxZoom={1.8}
          proOptions={{ hideAttribution: true }}
        >
          {/* Breadcrumb navigation, or the flow indicator when focused. */}
          <Panel position="top-left" className="!m-3">
            <div className="flex max-w-[70vw] flex-wrap items-center gap-1 rounded-lg border border-panel-border bg-neutral-950/90 px-2.5 py-1.5 font-mono text-[11px] shadow-lg backdrop-blur">
              {flowFocus ? (
                <>
                  <span className="font-semibold text-cyan-300">⊙ flow</span>
                  <span className="text-neutral-600">·</span>
                  <span className="truncate text-neutral-100">{flowName}</span>
                  {flow && (
                    <span className="ml-1.5 border-l border-panel-border pl-2 text-[10px] text-neutral-500">
                      {flow.callers} in · {flow.deps} out
                    </span>
                  )}
                  <button
                    onClick={() => setFlowFocus(null)}
                    className="ml-2 rounded border border-panel-border px-1.5 py-0.5 text-[10px] text-neutral-300 transition-colors hover:border-accent hover:text-accent"
                  >
                    ✕ exit flow
                  </button>
                </>
              ) : (
                <>
                  {crumbs.map((c, i) => {
                    const last = i === crumbs.length - 1;
                    return (
                      <span key={c.path} className="flex items-center gap-1">
                        {i > 0 && <span className="text-neutral-600">/</span>}
                        <button
                          onClick={() => !last && navigate(c.path)}
                          disabled={last}
                          className={
                            last
                              ? "font-semibold text-accent"
                              : "text-neutral-400 transition-colors hover:text-neutral-100"
                          }
                        >
                          {i === 0 ? "◆ root" : c.name}
                        </button>
                      </span>
                    );
                  })}
                  <span className="ml-1.5 border-l border-panel-border pl-2 text-[10px] text-neutral-600">{levelLabel}</span>
                </>
              )}
            </div>
          </Panel>

          {isFileLevel && !flowFocus && view.nodes.length === 0 && (
            <Panel position="top-right" className="!m-3 rounded-md border border-panel-border bg-neutral-950/90 px-2.5 py-1 font-mono text-[11px] text-neutral-500">
              loading functions…
            </Panel>
          )}

          {/* Flow mode: full path of the hovered file (screen-space, zoom-independent). */}
          {flowFocus && hoverPath && (
            <Panel position="bottom-center" className="!mb-4">
              <div className="flex max-w-[60vw] items-center gap-1.5 rounded-lg border border-cyan-500/40 bg-neutral-950/95 px-3 py-1.5 font-mono text-[11px] text-neutral-100 shadow-xl backdrop-blur">
                <span className="text-[10px] text-cyan-400/80">📄</span>
                <span className="truncate">{hoverPath}</span>
              </div>
            </Panel>
          )}

          <Background variant={BackgroundVariant.Dots} gap={24} size={1} color="#26262b" />
          <Controls showInteractive={false} className="!border-panel-border !bg-panel" />
          <MiniMap
            pannable
            zoomable
            maskColor="rgba(10,10,10,0.75)"
            nodeColor={minimapColor}
            style={{ background: "#111113", border: "1px solid #26262b" }}
          />
        </ReactFlow>
      </div>
    </CanvasActionsContext.Provider>
  );
}
