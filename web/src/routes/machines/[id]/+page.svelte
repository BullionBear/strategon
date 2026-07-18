<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { watchMachine } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { phaseLabel, isFailPhase } from '$lib/phases';

	let machine = $state<Machine | null>(null);
	let live = $state(false);
	let error = $state('');

	const id = $derived(page.params.id ?? '');

	onMount(() => {
		const ac = new AbortController();
		live = true;
		watchMachine(
			id,
			(m) => {
				machine = m;
				error = '';
			},
			ac.signal
		).finally(() => {
			live = false;
		});
		return () => ac.abort();
	});
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
			agent v{machine.agentVersion} · generation {machine.metadata?.generation ?? 0} ·
			{machine.reachable ? 'reachable' : 'unreachable'}
		</div>

		<h2 style="margin-top:1.75rem">Strategies</h2>
		<p class="muted">Desired vs actual. Diverging rows are highlighted.</p>

		{#if machine.strategies.length === 0}
			<p class="muted" style="margin-top:1rem">No strategies assigned. Use <a href="/deploy">Deploy</a>.</p>
		{:else}
			<div class="grid" style="margin-top:1rem">
				{#each machine.strategies as s (s.strategy)}
					<a
						class="panel strat"
						class:diverging={!s.converged}
						href="/machines/{id}/{s.strategy}"
					>
						<div class="row">
							<strong class="mono">{s.strategy}</strong>
							{#if s.converged}
								<span class="pill ok">converged</span>
							{:else if isFailPhase(s.phase)}
								<span class="pill bad">{phaseLabel(s.phase)}</span>
							{:else}
								<span class="pill lag">{phaseLabel(s.phase)}</span>
							{/if}
						</div>
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
						{#if s.pid}
							<p class="muted mono tiny" style="margin:0.5rem 0 0">pid {s.pid} · restarts {s.restartCount}</p>
						{/if}
					</a>
				{/each}
			</div>
		{/if}
	{/if}
</section>

<style>
	.head {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-top: 0.35rem;
	}
	.strat {
		display: block;
		color: inherit;
		text-decoration: none;
		transition: border-color 0.15s;
	}
	.strat:hover {
		border-color: var(--accent);
		text-decoration: none;
	}
	.strat.diverging {
		border-color: #e6c98a;
		background: linear-gradient(180deg, #fffaf0, var(--surface));
	}
	.row {
		display: flex;
		justify-content: space-between;
		align-items: center;
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
	.err {
		margin: 0.65rem 0 0;
		color: var(--danger);
		font-size: 0.82rem;
	}
</style>
