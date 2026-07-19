<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { client, watchMachine } from '$lib/api';
	import type { Machine, StrategyView } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { DeployPhase } from '$lib/gen/strategyplatform/v1/status_pb';
	import { HAPPY_PATH, FAIL_PATH, phaseLabel, happyIndex, isFailPhase } from '$lib/phases';

	let machine = $state<Machine | null>(null);
	let pendingGen = $state<bigint | number | null>(null);
	let busy = $state(false);
	let actionError = $state('');
	let live = $state(false);

	const id = $derived(page.params.id ?? '');
	const strat = $derived(page.params.strat ?? '');
	const view = $derived(
		machine?.strategies.find((s) => s.strategy === strat) ?? null
	) as StrategyView | null;

	const idx = $derived(happyIndex(view?.phase));
	const failing = $derived(isFailPhase(view?.phase));
	const tracking =
		$derived(
			pendingGen != null &&
				view != null &&
				Number(view.observedGeneration) < Number(pendingGen)
		);

	onMount(() => {
		const ac = new AbortController();
		live = true;
		watchMachine(id, (m) => (machine = m), ac.signal).finally(() => (live = false));
		return () => ac.abort();
	});

	$effect(() => {
		if (
			pendingGen != null &&
			view &&
			Number(view.observedGeneration) >= Number(pendingGen) &&
			view.phase === DeployPhase.HEALTHY
		) {
			pendingGen = null;
		}
	});

	async function rollback() {
		busy = true;
		actionError = '';
		try {
			const res = await client.rollback({ machineId: id, strategy: strat, targetVersion: '' });
			pendingGen = res.generation;
		} catch (e) {
			actionError = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function undeploy() {
		if (!confirm(`Undeploy ${strat} from ${id}? The process will receive SIGTERM, then SIGKILL if it does not exit.`)) {
			return;
		}
		busy = true;
		actionError = '';
		try {
			await client.undeploy({ machineId: id, strategy: strat });
			await goto(`/machines/${id}`);
		} catch (e) {
			actionError = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="fade-in">
	<p class="muted">
		<a href="/machines/{id}">← {id}</a>
	</p>
	<div class="head">
		<div>
			<h1 class="mono">{strat}</h1>
			<p class="muted">Deploy phase tracker — live from WatchMachine</p>
		</div>
		{#if live}
			<span class="pill ok"><span class="live-dot"></span> live</span>
		{/if}
	</div>

	{#if !view}
		<p class="muted" style="margin-top:1.25rem">Waiting for strategy state…</p>
	{:else}
		<div class="summary panel" style="margin-top:1.25rem">
			<div class="vs">
				<div>
					<span class="lbl">Desired</span>
					<span class="mono ver">{view.desiredArtifact?.version || '—'}</span>
					<span class="muted mono tiny">spec gen {view.specGeneration}</span>
				</div>
				<div class:mismatch={!view.converged}>
					<span class="lbl">Actual</span>
					<span class="mono ver">{view.runningArtifact?.version || '—'}</span>
					<span class="muted mono tiny">obs gen {view.observedGeneration}</span>
				</div>
				<div>
					<span class="lbl">Status</span>
					{#if view.converged}
						<span class="pill ok">converged</span>
					{:else if tracking}
						<span class="pill lag">deploying → gen {pendingGen}</span>
					{:else if failing}
						<span class="pill bad">{phaseLabel(view.phase)}</span>
					{:else}
						<span class="pill lag">diverging</span>
					{/if}
				</div>
			</div>
			{#if view.lastError}
				<p class="err mono">{view.lastError}</p>
			{/if}
			<div class="actions">
				<a class="btn secondary" href="/deploy?machine={id}&strategy={strat}">Deploy…</a>
				<button class="btn secondary" disabled={busy} onclick={rollback}>Rollback</button>
				<button class="btn secondary" disabled={busy} onclick={undeploy}>Undeploy</button>
			</div>
			{#if actionError}
				<p class="err mono">{actionError}</p>
			{/if}
			{#if view.schedules?.length}
				<div class="sched">
					<span class="lbl">Schedules</span>
					<ul>
						{#each view.schedules as s}
							<li class="mono tiny">
								{s.name}: {s.cronExpr} ({s.timezone})
							</li>
						{/each}
					</ul>
					<a class="muted tiny" href="/schedules">Edit…</a>
				</div>
			{/if}
		</div>

		<!-- Phase state machine visualization (FRONTEND.md §4.1) -->
		<ol class="pipeline" aria-label="Deploy phases">
			{#each HAPPY_PATH as phase, i}
				{@const done = !failing && idx > i}
				{@const current = !failing && idx === i}
				<li class:done class:current class:future={!done && !current}>
					<span class="node">{done ? '✓' : i + 1}</span>
					<span class="name">{phaseLabel(phase)}</span>
				</li>
				{#if i < HAPPY_PATH.length - 1}
					<li class="edge" class:lit={idx > i && !failing} aria-hidden="true"></li>
				{/if}
			{/each}
		</ol>

		{#if failing}
			<ol class="pipeline fail" aria-label="Failure branch">
				{#each FAIL_PATH as phase}
					{@const current = view.phase === phase}
					{@const done =
						view.phase === DeployPhase.ROLLED_BACK && phase === DeployPhase.ROLLING_BACK}
					<li class="bad" class:done class:current>
						<span class="node">{done || current ? '!' : '·'}</span>
						<span class="name">{phaseLabel(phase)}</span>
					</li>
					{#if phase === DeployPhase.ROLLING_BACK}
						<li class="edge lit bad" aria-hidden="true"></li>
					{/if}
				{/each}
				{#if view.phase === DeployPhase.FAILED}
					<li class="current bad">
						<span class="node">✗</span>
						<span class="name">Failed</span>
					</li>
				{/if}
			</ol>
		{/if}
	{/if}
</section>

<style>
	.head {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		margin-top: 0.35rem;
	}
	.vs {
		display: grid;
		grid-template-columns: repeat(3, 1fr);
		gap: 1rem;
	}
	.vs > div {
		display: flex;
		flex-direction: column;
		gap: 0.25rem;
	}
	.lbl {
		font-size: 0.72rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-muted);
	}
	.ver {
		font-size: 1.15rem;
		font-weight: 600;
	}
	.mismatch .ver {
		color: var(--lag);
	}
	.tiny {
		font-size: 0.75rem;
	}
	.sched {
		margin-top: 0.75rem;
		padding-top: 0.75rem;
		border-top: 1px solid var(--line, #e5e5e5);
	}
	.sched ul {
		margin: 0.25rem 0;
		padding-left: 1.1rem;
	}
	.err {
		margin: 0.75rem 0 0;
		color: var(--danger);
		font-size: 0.85rem;
	}
	.actions {
		display: flex;
		gap: 0.5rem;
		margin-top: 1rem;
	}

	.pipeline {
		list-style: none;
		padding: 0;
		margin: 2rem 0 0;
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.35rem;
	}
	.pipeline > li:not(.edge) {
		display: flex;
		flex-direction: column;
		align-items: center;
		gap: 0.35rem;
		min-width: 4.5rem;
		transition: transform 0.2s ease;
	}
	.pipeline > li.current {
		transform: translateY(-2px);
	}
	.node {
		width: 2rem;
		height: 2rem;
		border-radius: 50%;
		display: grid;
		place-items: center;
		font-family: var(--mono);
		font-size: 0.75rem;
		font-weight: 600;
		border: 2px solid var(--line);
		background: #fff;
		color: var(--ink-muted);
	}
	.done .node {
		border-color: var(--ok);
		background: #edf8f1;
		color: var(--ok);
	}
	.current .node {
		border-color: var(--accent);
		background: var(--accent);
		color: #fff;
		box-shadow: 0 0 0 4px rgba(13, 115, 119, 0.18);
		animation: pulse-dot 1.4s ease-in-out infinite;
	}
	.bad .node,
	.current.bad .node {
		border-color: var(--danger);
		background: #fff1f0;
		color: var(--danger);
		box-shadow: 0 0 0 4px rgba(180, 35, 24, 0.12);
		animation: none;
	}
	.name {
		font-size: 0.68rem;
		font-weight: 600;
		text-align: center;
		color: var(--ink-muted);
		max-width: 5.5rem;
		line-height: 1.2;
	}
	.current .name,
	.done .name {
		color: var(--ink);
	}
	.edge {
		flex: 1 1 1.25rem;
		min-width: 0.75rem;
		height: 2px;
		background: var(--line);
		margin-bottom: 1.1rem;
		align-self: center;
	}
	.edge.lit {
		background: var(--ok);
	}
	.edge.bad {
		background: var(--danger);
	}
	.pipeline.fail {
		margin-top: 1rem;
		padding-top: 1rem;
		border-top: 1px dashed #f0b4ae;
	}

	@media (max-width: 639px) {
		.vs {
			grid-template-columns: 1fr;
		}
		.head {
			flex-direction: column;
		}
		.actions {
			flex-wrap: wrap;
		}
		.actions .btn {
			flex: 1 1 auto;
		}
	}
</style>
