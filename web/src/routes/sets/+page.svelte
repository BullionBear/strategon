<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { AssignmentSet } from '$lib/gen/strategyplatform/v1/assignmentset_pb';
	import {
		memberPhaseLabel,
		memberRowKey,
		memberStatusOf,
		setGenerationLag,
		setPhaseClass,
		setStrategy
	} from '$lib/sets';

	let sets = $state<AssignmentSet[]>([]);
	let error = $state('');
	let loading = $state(true);
	let busy = $state('');

	async function refresh() {
		try {
			const res = await client.listAssignmentSets({});
			sets = res.sets;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}

	async function remove(name: string) {
		if (
			!confirm(
				`Delete AssignmentSet ${name}? The controller will undeploy member assignments, then remove the object.`
			)
		) {
			return;
		}
		busy = name;
		error = '';
		try {
			await client.deleteAssignmentSet({ name });
			await refresh();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = '';
		}
	}

	onMount(() => {
		void refresh();
		const poll = setInterval(() => void refresh(), 2000);
		return () => clearInterval(poll);
	});
</script>

<section class="fade-in">
	<h1>Assignment Sets</h1>
	<p class="muted">
		Multi-machine workloads owned by the control-plane orchestrator. Apply YAML with
		<span class="mono">strategon apply -f</span> — this page does not write member assignments.
	</p>

	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}
	{#if loading}
		<p class="muted" style="margin-top:1rem">Loading…</p>
	{:else if sets.length === 0}
		<p class="muted" style="margin-top:1.25rem">
			No AssignmentSets yet. Apply <span class="mono">examples/nats/cluster.yaml</span> with the
			CLI. The controller — not this page — writes each member's assignment
			(<span class="mono">member.name</span>).
		</p>
	{:else}
		<div class="list" style="margin-top:1.25rem">
			{#each sets as c (c.metadata?.name)}
				{@const name = c.metadata?.name ?? ''}
				{@const phase = c.status?.phase || 'Pending'}
				{@const gen = c.metadata?.generation ?? 0n}
				{@const obs = c.status?.observedGeneration ?? 0n}
				{@const catalog = setStrategy(c)}
				{@const lag = setGenerationLag(c)}
				<div class="panel set">
					<div class="head">
						<div>
							<h2 class="mono">{name}</h2>
							<p class="muted tiny">
								catalog <span class="mono">{catalog}</span>
								· artifact <span class="mono">{c.spec?.artifactVersion || '—'}</span>
								{#if c.spec?.config || c.spec?.configVersion}
									· config
									<span class="mono">
										{c.spec.config
											? `${c.spec.config}@${c.spec.configVersion || '—'}`
											: c.spec.configVersion}
									</span>
								{/if}
							</p>
						</div>
						<div class="head-right">
							<span class="pill {setPhaseClass(phase)}">{phase}</span>
							<button
								type="button"
								class="btn secondary"
								disabled={busy === name}
								onclick={() => remove(name)}
							>
								Delete
							</button>
						</div>
					</div>

					<div class="gens mono tiny">
						<span class:mismatch={lag}>generation {gen.toString()}</span>
						<span class="muted">→</span>
						<span class:mismatch={lag}>observed {obs.toString()}</span>
						{#if c.spec?.update}
							<span class="muted">
								· maxUnavailable {c.spec.update.maxUnavailable || 1}
								· waitReady {c.spec.update.waitReadySeconds || 60}s
							</span>
						{/if}
					</div>

					{#if c.status?.message}
						<p class="err mono tiny">{c.status.message}</p>
					{/if}

					<div class="fleet-table-wrap" style="margin-top:0.85rem">
						<table class="fleet-table members">
							<thead>
								<tr>
									<th>Machine</th>
									<th>Server</th>
									<th>Config</th>
									<th>Assignment</th>
									<th>Ready</th>
									<th>Converged</th>
								</tr>
							</thead>
							<tbody>
								{#each c.spec?.members ?? [] as srv (memberRowKey(srv.machine, srv.name))}
									{@const st = memberStatusOf(c, srv.machine, srv.name)}
									<tr>
										<td>
											<a class="row-link mono" href="/machines/{srv.machine}">{srv.machine}</a>
										</td>
										<td class="mono muted">{srv.name}</td>
										<td class="mono muted">
											{#if srv.config || srv.configVersion}
												{srv.config
													? `${srv.config}@${srv.configVersion || c.spec?.configVersion || '—'}`
													: srv.configVersion}
											{:else}
												—
											{/if}
										</td>
										<td>
											<a class="row-link mono" href="/machines/{srv.machine}/{srv.name}">
												{memberPhaseLabel(st?.phase)}
											</a>
										</td>
										<td>
											<span class="pill {st?.ready ? 'ok' : 'off'}">{st?.ready ? 'true' : 'false'}</span>
										</td>
										<td>
											<span class="pill {st?.converged ? 'ok' : 'lag'}"
												>{st?.converged ? 'true' : 'false'}</span
											>
										</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
					<div class="fleet-cards" style="margin-top:0.85rem">
						{#each c.spec?.members ?? [] as srv (memberRowKey(srv.machine, srv.name))}
							{@const st = memberStatusOf(c, srv.machine, srv.name)}
							<div class="fleet-card">
								<div class="card-top">
									<a class="mono" href="/machines/{srv.machine}"><strong>{srv.machine}</strong></a>
									<span class="pill {st?.ready ? 'ok' : 'off'}">{st?.ready ? 'ready' : 'not ready'}</span>
								</div>
								<div class="card-meta">
									<span class="mono">{srv.name}</span>
									{#if srv.config || srv.configVersion}
										<span class="mono">
											{srv.config
												? `${srv.config}@${srv.configVersion || c.spec?.configVersion || '—'}`
												: srv.configVersion}
										</span>
									{/if}
									<a class="mono" href="/machines/{srv.machine}/{srv.name}">{memberPhaseLabel(st?.phase)}</a>
									<span>{st?.converged ? 'converged' : 'diverged'}</span>
								</div>
							</div>
						{/each}
					</div>
				</div>
			{/each}
		</div>
	{/if}
</section>

<style>
	.list {
		display: grid;
		gap: 1rem;
	}
	.set h2 {
		margin: 0;
		font-size: 1.15rem;
	}
	.head {
		display: flex;
		justify-content: space-between;
		gap: 1rem;
		align-items: flex-start;
	}
	.head-right {
		display: flex;
		align-items: center;
		gap: 0.65rem;
		flex-shrink: 0;
	}
	.gens {
		margin-top: 0.65rem;
		display: flex;
		flex-wrap: wrap;
		gap: 0.4rem;
	}
	.mismatch {
		color: var(--warn);
		font-weight: 600;
	}
	.err {
		margin-top: 0.5rem;
		color: var(--danger);
	}
	.tiny {
		font-size: 0.78rem;
	}
	.members tbody tr {
		cursor: default;
	}
	.members tbody tr:hover {
		background: transparent;
	}
	@media (max-width: 720px) {
		.head {
			flex-direction: column;
		}
	}
	@media (min-width: 640px) {
		.fleet-cards {
			display: none !important;
		}
	}
</style>
