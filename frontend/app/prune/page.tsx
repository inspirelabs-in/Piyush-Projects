"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { gsap } from "gsap";

import {
  fetchBugs,
  fetchPrune,
  type Bug,
  type BugReport,
  type PruneCandidate,
  type PruneReport,
} from "../workspace/lib/api";

// --- terminal palette -------------------------------------------------------
const SEV: Record<string, { color: string; tag: string }> = {
  CRITICAL: { color: "#ff4d4d", tag: "CRIT" },
  HIGH: { color: "#ff9100", tag: "HIGH" },
  MEDIUM: { color: "#ffc400", tag: "MED" },
};
const GREEN = "#22c55e";

const PRUNE_TIER: Record<string, { color: string; label: string }> = {
  orphan_file: { color: "#ff6b6b", label: "orphan" },
  dead_cluster: { color: "#ff9100", label: "dead_cluster" },
  unused_export: { color: "#ffc400", label: "unused_export" },
  unused_function: { color: "#9aa0a6", label: "unused_fn" },
};

function sevOf(s: string) {
  return SEV[s] ?? { color: "#9aa0a6", tag: s.slice(0, 4) };
}

export default function PruneTerminal() {
  const router = useRouter();
  const [repo, setRepo] = useState<string | null>(null);
  const [prune, setPrune] = useState<PruneReport | null>(null);
  const [bugs, setBugs] = useState<BugReport | null>(null);
  const [pruneState, setPruneState] = useState<"loading" | "ready" | "error">("loading");
  const [bugState, setBugState] = useState<"loading" | "ready" | "error">("loading");
  const rootRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setRepo(new URLSearchParams(window.location.search).get("repo"));
  }, []);

  useEffect(() => {
    if (repo === null) return;
    const ctrl = new AbortController();
    setPruneState("loading");
    setBugState("loading");
    fetchPrune(repo, ctrl.signal)
      .then((r) => {
        setPrune(r);
        setPruneState("ready");
      })
      .catch((e) => e?.name !== "AbortError" && setPruneState("error"));
    fetchBugs(repo, ctrl.signal)
      .then((r) => {
        setBugs(r);
        setBugState("ready");
      })
      .catch((e) => e?.name !== "AbortError" && setBugState("error"));
    return () => ctrl.abort();
  }, [repo]);

  // Boot-sequence reveal.
  useEffect(() => {
    if (!rootRef.current) return;
    const ctx = gsap.context(() => {
      gsap.from(".boot-line", { opacity: 0, x: -8, duration: 0.4, stagger: 0.12, ease: "power2.out" });
    }, rootRef);
    return () => ctx.revert();
  }, []);

  // Fade findings in as each section resolves.
  useEffect(() => {
    if (bugState !== "ready" || !rootRef.current) return;
    gsap.fromTo(".bug-row", { opacity: 0, y: 6 }, { opacity: 1, y: 0, duration: 0.35, stagger: 0.05, ease: "power2.out" });
  }, [bugState]);
  useEffect(() => {
    if (pruneState !== "ready" || !rootRef.current) return;
    gsap.fromTo(".prune-row", { opacity: 0, y: 6 }, { opacity: 1, y: 0, duration: 0.3, stagger: 0.03, ease: "power2.out" });
  }, [pruneState]);

  const repoQuery = repo ? `?repo=${encodeURIComponent(repo)}` : "";
  const name = bugs?.name ?? prune?.name ?? "—";

  const sevCounts = useMemo(() => {
    const c = { CRITICAL: 0, HIGH: 0, MEDIUM: 0 };
    for (const b of bugs?.bugs ?? []) if (b.severity in c) c[b.severity as keyof typeof c]++;
    return c;
  }, [bugs]);

  const t1 = (bugs?.bugs ?? []).filter((b) => b.tier === "deterministic").length;
  const t2 = (bugs?.bugs ?? []).filter((b) => b.tier === "llm").length;
  const deadCount = prune?.candidates.length ?? 0;

  return (
    <div
      ref={rootRef}
      className="term-grid term-scanlines term-beam term-flicker relative h-full overflow-y-auto bg-black font-mono text-[13px] text-neutral-300 selection:bg-emerald-500/30"
    >
      {/* nav */}
      <header className="sticky top-0 z-10 flex items-center justify-between border-b border-emerald-500/20 bg-black/90 px-4 py-2 backdrop-blur">
        <button onClick={() => router.push("/")} className="flex items-center gap-2 text-[12px]">
          <span className="h-2 w-2 rounded-full bg-emerald-400 shadow-[0_0_10px_2px_#22c55e]" />
          <span className="font-semibold tracking-tight text-emerald-300 term-glow">synapse</span>
          <span className="text-emerald-500/50">/</span>
          <span className="text-neutral-400">diagnostics</span>
        </button>
        <div className="flex items-center gap-1.5 text-[11px]">
          {([["./docs", `/docs${repoQuery}`], ["./architecture", `/architecture${repoQuery}`], ["./workspace", `/workspace${repoQuery}`]] as const).map(
            ([label, href]) => (
              <button
                key={label}
                onClick={() => router.push(href)}
                className="rounded border border-emerald-500/20 px-2 py-1 text-emerald-300/80 transition-colors hover:border-emerald-400/60 hover:bg-emerald-500/10 hover:text-emerald-200"
              >
                {label}
              </button>
            ),
          )}
        </div>
      </header>

      <div className="relative z-[3] mx-auto max-w-5xl px-4 py-5">
        {/* boot header */}
        <div className="rounded-lg border border-emerald-500/20 bg-black/60 p-4 shadow-[0_0_30px_-12px_#22c55e]">
          <div className="boot-line text-emerald-400">
            <span className="text-emerald-500/70">[root@synapse_prune]</span>
            <span className="text-neutral-500">:</span>
            <span className="text-cyan-400">~</span>
            <span className="text-neutral-500">$ </span>
            <span className="text-neutral-200">./scan --repo {name} --tier1 --tier2 --adversarial</span>
          </div>
          <div className="boot-line mt-1 text-[12px] text-neutral-500">
            &gt; loading AST graph + semantic layer …{" "}
            <span className="text-emerald-400">ok</span>
          </div>
          <div className="boot-line mt-0.5 text-[12px] text-neutral-500">
            &gt; [tier 1] structural scan (cycles · resource-leaks · dead-code) …{" "}
            {pruneState === "ready" && bugState !== "loading" ? (
              <span className="text-emerald-400">done</span>
            ) : (
              <span className="text-amber-300">running<span className="term-cursor">_</span></span>
            )}
          </div>
          <div className="boot-line mt-0.5 text-[12px] text-neutral-500">
            &gt; [tier 2] adversarial llm red-team …{" "}
            {bugState === "loading" ? (
              <span className="text-amber-300">analyzing<span className="term-cursor">_</span></span>
            ) : bugState === "error" ? (
              <span className="text-red-400">offline</span>
            ) : (
              <span className="text-emerald-400">done</span>
            )}
          </div>
        </div>

        {/* HUD */}
        <div className="mt-4 grid grid-cols-2 gap-2 sm:grid-cols-6">
          <Stat label="CRITICAL" value={sevCounts.CRITICAL} color={SEV.CRITICAL.color} />
          <Stat label="HIGH" value={sevCounts.HIGH} color={SEV.HIGH.color} />
          <Stat label="MEDIUM" value={sevCounts.MEDIUM} color={SEV.MEDIUM.color} />
          <Stat label="DEAD CODE" value={deadCount} color="#ff9100" />
          <Stat label="T1 / T2" value={`${t1}/${t2}`} color={GREEN} />
          <Stat label="FILES" value={bugs?.scanned ?? prune?.code_files ?? 0} color="#67e8f9" />
        </div>

        {/* BUGS */}
        <SectionHeader title="critical_bugs.log" count={bugs?.bugs.length ?? 0} state={bugState} />
        {bugState === "loading" && (
          <Loading text="running structural + adversarial analysis — this can take a moment" />
        )}
        {bugState === "error" && <ErrorLine text="bug scan unavailable — is the backend on :8080?" />}
        {bugState === "ready" && (bugs?.bugs.length ?? 0) === 0 && (
          <CleanLine text="no critical bugs or anti-patterns detected" />
        )}
        <div className="mt-2 space-y-2">
          {(bugs?.bugs ?? []).map((b) => (
            <BugCard key={b.bug_id} bug={b} />
          ))}
        </div>

        {/* DEAD CODE */}
        <SectionHeader title="dead_code.log" count={deadCount} state={pruneState} />
        {pruneState === "loading" && <Loading text="reachability scan over the import graph" />}
        {pruneState === "error" && <ErrorLine text="prune scan unavailable" />}
        {pruneState === "ready" && deadCount === 0 && <CleanLine text="no orphaned files or symbols — graph fully reachable" />}
        <div className="mt-2 space-y-1">
          {(prune?.candidates ?? []).map((c, i) => (
            <PruneRow key={c.path + c.symbol + i} c={c} />
          ))}
        </div>

        {/* caveats */}
        {(bugs?.notes?.length || prune?.notes?.length) && (
          <div className="mt-6 rounded border border-amber-500/20 bg-amber-500/[0.04] px-3 py-2 text-[11px] leading-relaxed text-neutral-500">
            <span className="text-amber-300/80"># review, don&apos;t auto-apply</span>
            {[...(bugs?.notes ?? []), ...(prune?.notes ?? [])].map((n, i) => (
              <div key={i} className="mt-0.5">
                <span className="text-neutral-600">~ </span>
                {n}
              </div>
            ))}
          </div>
        )}
        <div className="mt-4 pb-8 text-[11px] text-neutral-700">
          [root@synapse_prune]:~$ <span className="term-cursor">█</span>
        </div>
      </div>
    </div>
  );
}

function Stat({ label, value, color }: { label: string; value: number | string; color: string }) {
  return (
    <div className="rounded border border-neutral-800 bg-black/60 px-2.5 py-2">
      <div className="text-[9px] uppercase tracking-wider text-neutral-600">{label}</div>
      <div className="mt-0.5 text-[20px] font-bold leading-none term-glow" style={{ color }}>
        {value}
      </div>
    </div>
  );
}

function SectionHeader({ title, count, state }: { title: string; count: number; state: string }) {
  return (
    <div className="mt-7 flex items-center gap-2 border-b border-emerald-500/20 pb-1.5">
      <span className="text-emerald-500/60">▌</span>
      <span className="text-[12px] font-semibold tracking-wide text-emerald-300">{title}</span>
      {state === "ready" && (
        <span className="rounded bg-neutral-900 px-1.5 py-0.5 text-[10px] text-neutral-500">{count} record{count === 1 ? "" : "s"}</span>
      )}
      <span className="ml-auto text-[10px] text-neutral-700">
        {state === "loading" ? "● scanning" : state === "error" ? "● error" : "● idle"}
      </span>
    </div>
  );
}

function Loading({ text }: { text: string }) {
  return (
    <div className="mt-2 flex items-center gap-2 rounded border border-neutral-800 bg-black/60 px-3 py-2.5 text-[12px] text-amber-300/90">
      <span className="inline-flex gap-1">
        <span className="dot-pulse h-1.5 w-1.5 rounded-full bg-amber-400" />
        <span className="dot-pulse h-1.5 w-1.5 rounded-full bg-amber-400 [animation-delay:0.15s]" />
        <span className="dot-pulse h-1.5 w-1.5 rounded-full bg-amber-400 [animation-delay:0.3s]" />
      </span>
      {text}
      <span className="term-cursor">_</span>
    </div>
  );
}

function ErrorLine({ text }: { text: string }) {
  return <div className="mt-2 rounded border border-red-500/30 bg-red-500/[0.06] px-3 py-2 text-[12px] text-red-300">! {text}</div>;
}

function CleanLine({ text }: { text: string }) {
  return (
    <div className="mt-2 rounded border border-emerald-500/30 bg-emerald-500/[0.05] px-3 py-2.5 text-[12px] text-emerald-300">
      <span className="term-glow">✓</span> {text}
    </div>
  );
}

function BugCard({ bug }: { bug: Bug }) {
  const [open, setOpen] = useState(bug.severity === "CRITICAL");
  const s = sevOf(bug.severity);
  const loc = bug.location;
  const lines = loc.line_start ? `:${loc.line_start}${loc.line_end && loc.line_end !== loc.line_start ? `-${loc.line_end}` : ""}` : "";
  return (
    <div
      className="bug-row overflow-hidden rounded-md border bg-black/60 transition-colors"
      style={{ borderColor: s.color + "40" }}
    >
      <button onClick={() => setOpen((o) => !o)} className="flex w-full flex-wrap items-center gap-2 px-3 py-2 text-left">
        <span
          className="shrink-0 rounded px-1.5 py-0.5 text-[10px] font-bold tracking-wider text-black term-glow"
          style={{ backgroundColor: s.color }}
        >
          {s.tag}
        </span>
        <span className="shrink-0 text-[10px] text-neutral-600">{bug.bug_id}</span>
        <span
          className="shrink-0 rounded border px-1 py-0.5 text-[9px] uppercase"
          style={{ borderColor: bug.tier === "llm" ? "#a78bfa66" : "#22c55e66", color: bug.tier === "llm" ? "#c4b5fd" : "#86efac" }}
        >
          {bug.tier === "llm" ? "T2·llm" : "T1·det"}
        </span>
        <span className="shrink-0 rounded bg-neutral-900 px-1.5 py-0.5 text-[9px] text-neutral-500">{bug.category}</span>
        <span className="min-w-0 flex-1 truncate text-[13px] font-medium text-neutral-100">{bug.title}</span>
        <span className="shrink-0 select-none text-[10px] text-neutral-600">{open ? "▾" : "▸"}</span>
      </button>
      <div className="border-t px-3 py-1.5 text-[11px] text-neutral-500" style={{ borderColor: s.color + "22" }}>
        <span className="text-neutral-600">@ </span>
        <span className="text-cyan-300/90">{loc.file}{lines}</span>
        {loc.entity && <span className="text-neutral-500"> :: <span className="text-indigo-300/90">{loc.entity}</span></span>}
      </div>
      {open && (
        <div className="space-y-2 border-t px-3 py-2.5 text-[12px] leading-relaxed" style={{ borderColor: s.color + "22" }}>
          <Field glyph="▸" label="issue" color={s.color} text={bug.finding.issue} />
          <Field glyph="⚡" label="impact" color="#ff9100" text={bug.finding.impact} />
          <Field glyph="✓" label="fix" color={GREEN} text={bug.finding.fix} />
          {bug.context_nodes.length > 0 && (
            <div className="flex flex-wrap items-center gap-1 pt-0.5">
              <span className="text-[9px] uppercase tracking-wider text-neutral-600">nodes</span>
              {bug.context_nodes.slice(0, 8).map((n) => (
                <code key={n} className="rounded bg-neutral-900 px-1.5 py-0.5 text-[10px] text-neutral-400">{n.split("/").pop()}</code>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function Field({ glyph, label, color, text }: { glyph: string; label: string; color: string; text: string }) {
  if (!text) return null;
  return (
    <div className="flex gap-2">
      <span className="shrink-0 font-bold" style={{ color }}>{glyph}</span>
      <div className="min-w-0">
        <span className="mr-1.5 text-[9px] uppercase tracking-wider" style={{ color }}>{label}</span>
        <span className="text-neutral-300">{text}</span>
      </div>
    </div>
  );
}

function PruneRow({ c }: { c: PruneCandidate }) {
  const meta = PRUNE_TIER[c.tier] ?? { color: "#9aa0a6", label: c.tier };
  return (
    <div className="prune-row flex flex-wrap items-center gap-2 rounded border border-neutral-800/80 bg-black/50 px-3 py-1.5">
      <span className="h-1.5 w-1.5 shrink-0 rounded-full" style={{ backgroundColor: meta.color }} />
      <span className="shrink-0 text-[9px] uppercase tracking-wider" style={{ color: meta.color }}>{meta.label}</span>
      <span className="min-w-0 break-all text-[12px] text-neutral-200">
        {c.path}
        {c.symbol && <span className="text-indigo-300/90"> :: {c.symbol}</span>}
      </span>
      {c.uncertain && <span className="shrink-0 text-[9px] text-amber-300/80">⚠ verify</span>}
      <span className="ml-auto shrink-0 text-[9px] text-neutral-600">{c.confidence}</span>
    </div>
  );
}
