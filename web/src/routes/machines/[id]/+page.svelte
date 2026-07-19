<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import { client, watchMachine } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { phaseLabel, isFailPhase } from '$lib/phases';

	let machine = $state<Machine | null>(null);
	let live = $state(false);
	let error = $state('');
	let busy = $state('');

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

	async function undeploy(strategy: string) {
		if (
			!confirm(
				`Undeploy ${strategy} from ${id}? The process will drain and the lease will be released.`
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
		</div>

		<h2 style="margin-top:1.75rem">Strategies</h2>
		<p class="muted">Desired vs actual. Diverging rows are highlighted.</p>

		{#if machine.strategies.length === 0}
			<p class="muted" style="margin-top:1rem">No strategies assigned. Use <a href="/deploy">Deploy</a>.</p>
		{:else}
			<div class="grid" style="margin-top:1rem">
				{#each machine.strategies as s (s.strategy)}
					<div class="panel strat" class:diverging={!s.converged}>
						<div class="row">
							<a class="title" href="/machines/{id}/{s.strategy}">
								<strong class="mono">{s.strategy}</strong>
							</a>
							{#if s.converged}
								<span class="pill ok">converged</span>
							{:else if isFailPhase(s.phase)}
								<span class="pill bad">{phaseLabel(s.phase)}</span>
							{:else}
								<span class="pill lag">{phaseLabel(s.phase)}</span>
							{/if}
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
							{#if s.pid}
								<p class="muted mono tiny" style="margin:0.5rem 0 0">
									pid {s.pid} · restarts {s.restartCount}
								</p>
							{/if}
						</a>
						<div class="actions">
							<button
								class="btn secondary"
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
	.actions {
		display: flex;
		justify-content: flex-end;
		margin-top: 0.35rem;
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
