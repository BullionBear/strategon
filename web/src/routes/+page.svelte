<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';

	let machines = $state<Machine[]>([]);
	let error = $state('');
	let loading = $state(true);

	async function refresh() {
		try {
			const res = await client.listMachines({ pageSize: 100 });
			machines = res.machines;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			loading = false;
		}
	}

	onMount(() => {
		refresh();
		const t = setInterval(refresh, 2000);
		return () => clearInterval(t);
	});
</script>

<section class="fade-in">
	<h1>Fleet</h1>
	<p class="muted">Machines registered with the control plane. Polls every 2s.</p>

	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}
	{#if loading}
		<p class="muted" style="margin-top:1rem">Loading…</p>
	{:else if machines.length === 0}
		<p class="muted" style="margin-top:1.25rem">
			No machines yet. Start an agent pointing at the control plane.
		</p>
	{:else}
		<div class="grid machines" style="margin-top:1.25rem">
			{#each machines as m (m.metadata?.uid ?? m.metadata?.name)}
				{@const id = m.metadata?.uid || m.metadata?.name || '?'}
				<a class="panel machine" href="/machines/{id}">
					<div class="row">
						<strong class="mono">{id}</strong>
						{#if m.reachable}
							<span class="pill ok"><span class="live-dot"></span> reachable</span>
						{:else}
							<span class="pill off"><span class="live-dot off"></span> unreachable</span>
						{/if}
					</div>
					<div class="muted mono" style="margin-top:0.45rem;font-size:0.8rem">
						agent v{m.agentVersion} · gen {m.metadata?.generation ?? 0} ·
						{m.strategies.length} strateg{m.strategies.length === 1 ? 'y' : 'ies'}
					</div>
					{#if m.strategies.length}
						<ul class="strats">
							{#each m.strategies as s}
								<li>
									<span class="mono">{s.strategy}</span>
									{#if s.converged}
										<span class="pill ok">converged</span>
									{:else}
										<span class="pill lag">diverging</span>
									{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</a>
			{/each}
		</div>
	{/if}
</section>

<style>
	.machine {
		display: block;
		color: inherit;
		text-decoration: none;
		transition: border-color 0.15s, transform 0.15s;
	}
	.machine:hover {
		border-color: var(--accent);
		transform: translateY(-1px);
		text-decoration: none;
	}
	.row {
		display: flex;
		justify-content: space-between;
		align-items: center;
		gap: 0.5rem;
	}
	.strats {
		list-style: none;
		padding: 0;
		margin: 0.75rem 0 0;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
	}
	.strats li {
		display: flex;
		justify-content: space-between;
		align-items: center;
		font-size: 0.88rem;
	}
</style>
