<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { StrategySlotView } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { formatBytes } from '$lib/fleet';
	import { MIN_REAP_STRATEGIES_AGENT_VERSION } from '$lib/workdir';

	interface Props {
		machineId: string;
		reachable?: boolean;
		agentVersion?: number;
	}
	let { machineId, reachable = false, agentVersion = 0 }: Props = $props();

	let slots = $state<StrategySlotView[]>([]);
	let totalBytes = $state(0n);
	let error = $state('');
	let busy = $state('');

	const canReap = $derived(reachable && agentVersion >= MIN_REAP_STRATEGIES_AGENT_VERSION);

	async function load() {
		if (!machineId) return;
		try {
			const res = await client.listStrategySlots({ machineId });
			slots = res.slots;
			totalBytes = res.totalBytes;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 10000);
		return () => clearInterval(t);
	});

	$effect(() => {
		if (machineId) load();
	});

	async function reap(name: string) {
		if (
			!confirm(
				`Delete orphan slot "${name}"? This removes releases, work, and logs and cannot be undone.`
			)
		) {
			return;
		}
		busy = name;
		error = '';
		try {
			const res = await client.reapStrategies({ machineId, strategies: [name] });
			const one = res.results.find((r) => r.strategy === name);
			if (one && !one.removed && one.error) {
				error = one.error;
			}
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = '';
		}
	}
</script>

<section class="slots">
	<div class="head">
		<h2>Disk slots</h2>
		{#if slots.length}
			<span class="muted mono tiny">total {formatBytes(totalBytes)}</span>
		{/if}
	</div>
	<p class="muted">
		On-disk strategy directories under the agent base. Undeploy keeps them; reap deletes an orphan
		slot (not assigned, process stopped).
	</p>

	{#if error}
		<p class="pill bad">{error}</p>
	{/if}

	{#if slots.length === 0}
		<p class="muted" style="margin-top:0.75rem">No strategy slots reported by the agent.</p>
	{:else}
		<div class="grid" style="margin-top:0.75rem">
			{#each slots as s (s.strategy)}
				<div class="panel row">
					<div class="top">
						<strong class="mono">{s.strategy}</strong>
						{#if s.assigned}
							<span class="pill ok">assigned</span>
						{:else}
							<span class="pill lag">orphan</span>
						{/if}
					</div>
					<div class="vs muted mono tiny">
						<span>size {s.sizeBytes ? formatBytes(s.sizeBytes) : '—'}</span>
						<span>current {s.currentVersion || '—'}</span>
					</div>
					{#if !s.assigned}
						<button
							class="btn danger"
							type="button"
							disabled={busy !== '' || !canReap}
							onclick={() => reap(s.strategy)}
						>
							{busy === s.strategy ? 'Reaping…' : 'Reap'}
						</button>
					{/if}
				</div>
			{/each}
		</div>
		{#if !canReap}
			<p class="muted tiny" style="margin-top:0.5rem">
				Reap needs a reachable agent with capability version ≥ {MIN_REAP_STRATEGIES_AGENT_VERSION}.
			</p>
		{/if}
	{/if}
</section>

<style>
	.slots {
		margin-top: 1.75rem;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.head h2 {
		margin: 0;
	}
	.grid {
		display: grid;
		gap: 0.75rem;
	}
	.row .top {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}
	.vs {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
		margin-top: 0.35rem;
	}
	.tiny {
		font-size: 0.8rem;
	}
</style>
