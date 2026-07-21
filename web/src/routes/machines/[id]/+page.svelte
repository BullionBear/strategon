<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { client, watchMachine } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { phaseLabel, isFailPhase } from '$lib/phases';
	import {
		barTone,
		cpuPercent,
		formatBytes,
		formatClock,
		formatUptime,
		memoryPercent
	} from '$lib/fleet';
	import Sparkline from '$lib/Sparkline.svelte';

	let machine = $state<Machine | null>(null);
	let live = $state(false);
	let error = $state('');
	let busy = $state('');
	let now = $state(Date.now());
	let cpuSeries = $state<number[]>([]);
	let memSeries = $state<number[]>([]);
	let stratSeries = $state<Record<string, number[]>>({});

	const id = $derived(page.params.id ?? '');
	const cpu = $derived(machine ? cpuPercent(machine) : null);
	const mem = $derived(machine ? memoryPercent(machine) : null);
	const grafanaBase =
		(typeof import.meta !== 'undefined' && import.meta.env?.VITE_GRAFANA_URL) || '';

	async function loadMetrics() {
		if (!id) return;
		try {
			const res = await client.getMachineMetrics({ machineId: id, rangeSeconds: 3600n });
			cpuSeries = res.samples.map((s) => s.cpuPercent);
			memSeries = res.samples.map((s) => Number(s.memBytes));
			const next: Record<string, number[]> = {};
			for (const s of machine?.strategies ?? []) {
				const pr = await client.getMachineMetrics({
					machineId: id,
					strategy: s.strategy,
					rangeSeconds: 3600n
				});
				next[s.strategy] = pr.samples.map((p) => p.cpuPercent);
			}
			stratSeries = next;
		} catch {
			/* sparkline is best-effort */
		}
	}

	onMount(() => {
		const ac = new AbortController();
		live = true;
		let metricsLoaded = false;
		watchMachine(
			id,
			(m) => {
				machine = m;
				error = '';
				now = Date.now();
				if (!metricsLoaded) {
					metricsLoaded = true;
					loadMetrics();
				}
			},
			ac.signal
		).finally(() => {
			live = false;
		});
		const metricsTimer = setInterval(loadMetrics, 30000);
		const clockTimer = setInterval(() => (now = Date.now()), 1000);
		return () => {
			ac.abort();
			clearInterval(metricsTimer);
			clearInterval(clockTimer);
		};
	});

	async function stop(strategy: string) {
		busy = strategy;
		error = '';
		try {
			await client.stop({ machineId: id, strategy });
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = '';
		}
	}

	async function start(strategy: string) {
		busy = strategy;
		error = '';
		try {
			await client.start({ machineId: id, strategy });
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = '';
		}
	}

	async function undeploy(strategy: string) {
		if (
			!confirm(
				`Undeploy ${strategy} from ${id}? This deletes the deployment record and its logs/history from the UI. Use Stop to halt the process while keeping history.`
			)
		) {
			return;
		}
		busy = strategy;
		error = '';
		try {
			await client.undeploy({ machineId: id, strategy });
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = '';
		}
	}
</script>

<section class="fade-in">
	<p class="muted"><a href="/">← Fleet</a></p>
	<div class="head">
		<h1 class="mono">{id}</h1>
		{#if live}
			<span class="pill ok"><span class="live-dot"></span> watching</span>
		{:else}
			<span class="pill off">stream idle</span>
		{/if}
	</div>

	{#if error}
		<p class="pill bad">{error}</p>
	{/if}

	{#if !machine}
		<p class="muted" style="margin-top:1rem">Connecting…</p>
	{:else}
		<div class="meta muted mono" style="margin-top:0.5rem">
			agent v{machine.agentVersion}
			{#if machine.agentBuildVersion}
				· build {machine.agentBuildVersion}{/if}
			· generation {machine.metadata?.generation ?? 0} ·
			{machine.reachable ? 'reachable' : 'unreachable'}
			{#if machine.spec?.numCpus}
				· {machine.spec.numCpus} cpu{/if}
			{#if machine.spec?.memoryTotalBytes}
				· {formatBytes(machine.spec.memoryTotalBytes)} ram{/if}
			{#if machine.spec?.kernelVersion}
				· {machine.spec.kernelVersion}{/if}
		</div>

		<div class="resources panel" style="margin-top:1.25rem">
			<div class="res-col">
				<div class="res-head">
					<span class="lbl">CPU</span>
					<span class="mono pct">{cpu == null ? '—' : `${Math.round(cpu)}%`}</span>
				</div>
				<div class="mini-bar {barTone(cpu)}">
					<span class="track"><span class="fill" style="width: {cpu ?? 0}%"></span></span>
				</div>
				<Sparkline values={cpuSeries} width={220} height={36} />
				<span class="muted tiny">1h trend</span>
			</div>
			<div class="res-col">
				<div class="res-head">
					<span class="lbl">Memory</span>
					<span class="mono pct">{mem == null ? '—' : `${Math.round(mem)}%`}</span>
				</div>
				<div class="mini-bar {barTone(mem)}">
					<span class="track"><span class="fill" style="width: {mem ?? 0}%"></span></span>
				</div>
				<Sparkline
					values={memSeries}
					width={220}
					height={36}
					stroke="var(--warn)"
					fill="rgba(184, 110, 0, 0.12)"
				/>
				<span class="muted tiny">
					{formatBytes(machine.lastResources?.memoryUsedBytes)}
					/ {formatBytes(machine.lastResources?.memoryTotalBytes || machine.spec?.memoryTotalBytes)}
				</span>
			</div>
			{#if grafanaBase}
				<div class="res-col grafana">
					<a
						class="btn secondary"
						href="{grafanaBase}?var-machine={encodeURIComponent(id)}"
						target="_blank"
						rel="noreferrer"
					>
						Open in Grafana
					</a>
				</div>
			{/if}
		</div>

		<h2 style="margin-top:1.75rem">Strategies</h2>
		<p class="muted">Desired vs actual. Diverging rows are highlighted.</p>

		{#if machine.strategies.length === 0}
			<p class="muted" style="margin-top:1rem">No strategies assigned. Use <a href="/deploy">Deploy</a>.</p>
		{:else}
			<div class="grid" style="margin-top:1rem">
				{#each machine.strategies as s (s.strategy)}
					<div class="panel strat" class:diverging={!s.converged && !s.stopped}>
						<div class="row">
							<a class="title" href="/machines/{id}/{s.strategy}">
								<strong class="mono">{s.strategy}</strong>
							</a>
							<div class="badges">
								{#if s.stopped}
									<span class="pill off">stopped</span>
								{/if}
								{#if s.converged}
									<span class="pill ok">converged</span>
								{:else if isFailPhase(s.phase)}
									<span class="pill bad">{phaseLabel(s.phase)}</span>
								{:else}
									<span class="pill lag">{phaseLabel(s.phase)}</span>
								{/if}
							</div>
						</div>
						<a class="body" href="/machines/{id}/{s.strategy}">
							<div class="vs">
								<div>
									<span class="lbl">Desired</span>
									<span class="mono">{s.desiredArtifact?.version || '—'}</span>
									<span class="muted mono tiny">spec gen {s.specGeneration}</span>
								</div>
								<div class:mismatch={!s.converged}>
									<span class="lbl">Actual</span>
									<span class="mono">{s.runningArtifact?.version || '—'}</span>
									<span class="muted mono tiny">obs gen {s.observedGeneration}</span>
								</div>
							</div>
							{#if s.lastError}
								<p class="err mono">{s.lastError}</p>
							{/if}
							<p class="life muted mono tiny">
								{#if s.deployedAt}
									deployed {formatClock(s.deployedAt)} ·
								{/if}
								{#if s.startedAt}
									up {formatUptime(s.startedAt, now)} ·
								{/if}
								restarts {s.restartCount}
								{#if s.pid}
									· pid {s.pid}{/if}
								· {phaseLabel(s.phase)}
							</p>
							{#if s.cpuPercent || s.rssBytes}
								<div class="proc-res">
									<div class="mini-bar {barTone(s.cpuPercent || null)}">
										<span class="track"
											><span class="fill" style="width: {Math.min(s.cpuPercent || 0, 100)}%"
											></span></span
										>
										<span class="pct">{Math.round(s.cpuPercent || 0)}% cpu</span>
									</div>
									<span class="muted mono tiny">{formatBytes(s.rssBytes)} rss</span>
									<Sparkline values={stratSeries[s.strategy] ?? []} width={100} height={24} />
								</div>
							{/if}
						</a>
						<div class="actions">
							{#if s.stopped}
								<button
									class="btn secondary"
									disabled={busy === s.strategy}
									onclick={() => start(s.strategy)}
								>
									Start
								</button>
							{:else}
								<button
									class="btn secondary"
									disabled={busy === s.strategy}
									onclick={() => stop(s.strategy)}
								>
									Stop
								</button>
							{/if}
							<button
								class="btn danger"
								disabled={busy === s.strategy}
								onclick={() => undeploy(s.strategy)}
							>
								Undeploy
							</button>
						</div>
					</div>
				{/each}
			</div>
		{/if}
	{/if}
</section>

<style>
	.head {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.75rem;
		margin-top: 0.35rem;
	}
	.head h1 {
		word-break: break-all;
	}
	.meta {
		word-break: break-word;
	}
	.resources {
		display: grid;
		grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
		gap: 1.25rem;
		align-items: start;
	}
	.res-col {
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
	}
	.res-head {
		display: flex;
		justify-content: space-between;
		align-items: baseline;
	}
	.grafana {
		justify-content: center;
	}
	.strat {
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
		transition: border-color 0.15s;
	}
	.strat:hover {
		border-color: var(--accent);
	}
	.strat.diverging {
		border-color: #e6c98a;
		background: linear-gradient(180deg, #fffaf0, var(--surface));
	}
	.strat a.title,
	.strat a.body {
		color: inherit;
		text-decoration: none;
	}
	.strat a.title:hover,
	.strat a.body:hover {
		text-decoration: none;
	}
	.row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
	}
	.badges {
		display: flex;
		align-items: center;
		gap: 0.35rem;
		flex-wrap: wrap;
	}
	.actions {
		display: flex;
		justify-content: flex-end;
		gap: 0.5rem;
		margin-top: 0.35rem;
		flex-wrap: wrap;
	}
	.vs {
		display: grid;
		grid-template-columns: 1fr 1fr;
		gap: 0.75rem;
		margin-top: 0.85rem;
	}
	.vs > div {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
	}
	.vs .mismatch .mono:first-of-type {
		color: var(--lag);
		font-weight: 600;
	}
	.lbl {
		font-size: 0.72rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-muted);
	}
	.tiny {
		font-size: 0.75rem;
	}
	.life {
		margin: 0.55rem 0 0;
	}
	.proc-res {
		display: flex;
		align-items: center;
		gap: 0.65rem;
		margin-top: 0.55rem;
		flex-wrap: wrap;
	}
	.proc-res .mini-bar {
		min-width: 110px;
	}
	.err {
		margin: 0.65rem 0 0;
		color: var(--danger);
		font-size: 0.82rem;
	}

	@media (max-width: 639px) {
		.vs {
			grid-template-columns: 1fr;
		}
		.actions {
			justify-content: stretch;
		}
		.actions .btn {
			width: 100%;
		}
	}
</style>
