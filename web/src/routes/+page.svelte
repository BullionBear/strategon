<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { client } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import {
		barTone,
		compareMachines,
		cpuPercent,
		formatHeartbeat,
		groupByRegion,
		machineId,
		machineStatus,
		memoryPercent,
		statusPillClass,
		type SortDir,
		type SortKey
	} from '$lib/fleet';

	let machines = $state<Machine[]>([]);
	let error = $state('');
	let loading = $state(true);
	let sortKey = $state<SortKey>('status');
	let sortDir = $state<SortDir>('asc');
	let collapsedRegions = $state<Record<string, boolean>>({});
	let mobileOpenRegion = $state<string | null>(null);
	let now = $state(Date.now());

	/** Fleet modal build version — for version-drift reminder only (no actions). */
	const fleetBuildMode = $derived.by(() => {
		const counts = new Map<string, number>();
		for (const m of machines) {
			const v = m.agentBuildVersion?.trim();
			if (!v) continue;
			counts.set(v, (counts.get(v) ?? 0) + 1);
		}
		let mode = '';
		let best = 0;
		for (const [v, n] of counts) {
			if (n > best) {
				best = n;
				mode = v;
			}
		}
		return mode;
	});

	function isBuildDrift(m: Machine): boolean {
		const v = m.agentBuildVersion?.trim();
		return !!v && !!fleetBuildMode && v !== fleetBuildMode;
	}

	const sorted = $derived(
		[...machines].sort((a, b) => compareMachines(a, b, sortKey, sortDir))
	);

	const groups = $derived(groupByRegion(sorted));

	function setSort(key: SortKey) {
		if (sortKey === key) {
			sortDir = sortDir === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			sortDir = key === 'status' || key === 'name' ? 'asc' : 'desc';
		}
	}

	function sortMark(key: SortKey): string {
		if (sortKey !== key) return '↕';
		return sortDir === 'asc' ? '↑' : '↓';
	}

	function isRegionOpen(region: string, index: number): boolean {
		if (collapsedRegions[region] === true) return false;
		if (collapsedRegions[region] === false) return true;
		return index === 0 || groups.length <= 3;
	}

	function toggleRegion(region: string, index: number) {
		const open = isRegionOpen(region, index);
		collapsedRegions = { ...collapsedRegions, [region]: open };
	}

	function isMobileRegionOpen(region: string, index: number): boolean {
		if (mobileOpenRegion === null) return index === 0;
		return mobileOpenRegion === region;
	}

	function toggleMobileRegion(region: string) {
		mobileOpenRegion = mobileOpenRegion === region ? '' : region;
	}

	function goMachine(m: Machine) {
		goto(`/machines/${machineId(m)}`);
	}

	async function refresh() {
		try {
			const res = await client.listMachines({ pageSize: 100 });
			machines = res.machines;
			error = '';
			now = Date.now();
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
		{#each groups as group, gi (group.region)}
			<section class="region-section">
				<button
					type="button"
					class="region-toggle desktop-only"
					aria-expanded={isRegionOpen(group.region, gi)}
					onclick={() => toggleRegion(group.region, gi)}
				>
					<span class="chev" aria-hidden="true">▾</span>
					<span>{group.region}</span>
					<span class="count">{group.machines.length}</span>
				</button>
				<button
					type="button"
					class="region-toggle mobile-only"
					aria-expanded={isMobileRegionOpen(group.region, gi)}
					onclick={() => toggleMobileRegion(group.region)}
				>
					<span class="chev" aria-hidden="true">▾</span>
					<span>{group.region}</span>
					<span class="count">{group.machines.length}</span>
				</button>

				{#if isRegionOpen(group.region, gi)}
					<div class="fleet-table-wrap">
						<table class="fleet-table">
							<thead>
								<tr>
									<th
										class="sortable"
										class:sorted={sortKey === 'name'}
										onclick={() => setSort('name')}
									>
										Machine <span class="sort-ind">{sortMark('name')}</span>
									</th>
									<th
										class="sortable"
										class:sorted={sortKey === 'status'}
										onclick={() => setSort('status')}
									>
										Status <span class="sort-ind">{sortMark('status')}</span>
									</th>
									<th
										class="sortable"
										class:sorted={sortKey === 'reachable'}
										onclick={() => setSort('reachable')}
									>
										Reachable <span class="sort-ind">{sortMark('reachable')}</span>
									</th>
									<th
										class="sortable col-p3"
										class:sorted={sortKey === 'agent'}
										onclick={() => setSort('agent')}
									>
										Agent <span class="sort-ind">{sortMark('agent')}</span>
									</th>
									<th
										class="sortable"
										class:sorted={sortKey === 'cpu'}
										onclick={() => setSort('cpu')}
									>
										CPU <span class="sort-ind">{sortMark('cpu')}</span>
									</th>
									<th
										class="sortable"
										class:sorted={sortKey === 'memory'}
										onclick={() => setSort('memory')}
									>
										Memory <span class="sort-ind">{sortMark('memory')}</span>
									</th>
									<th
										class="sortable"
										class:sorted={sortKey === 'strategies'}
										onclick={() => setSort('strategies')}
									>
										Strategies <span class="sort-ind">{sortMark('strategies')}</span>
									</th>
									<th
										class="sortable col-p3"
										class:sorted={sortKey === 'heartbeat'}
										onclick={() => setSort('heartbeat')}
									>
										Heartbeat <span class="sort-ind">{sortMark('heartbeat')}</span>
									</th>
								</tr>
							</thead>
							<tbody>
								{#each group.machines as m (machineId(m))}
									{@const id = machineId(m)}
									{@const st = machineStatus(m)}
									{@const cpu = cpuPercent(m)}
									{@const mem = memoryPercent(m)}
									{@const drift = isBuildDrift(m)}
									<tr
										onclick={() => goMachine(m)}
										onkeydown={(e) => {
											if (e.key === 'Enter' || e.key === ' ') {
												e.preventDefault();
												goMachine(m);
											}
										}}
										tabindex="0"
										role="link"
									>
										<td>
											<a class="row-link mono" href="/machines/{id}" onclick={(e) => e.stopPropagation()}
												>{id}</a
											>
											{#if drift}
												<span
													class="pill drift"
													style="margin-left:0.35rem"
													title="Build differs from the fleet majority — reminder only"
													>drift</span
												>
											{/if}
										</td>
										<td>
											<span class="pill {statusPillClass(st.kind)}">
												<span class="live-dot" class:off={st.kind === 'unreachable'}></span>
												{st.label}
											</span>
										</td>
										<td>
											{#if m.reachable}
												<span class="pill ok">yes</span>
											{:else}
												<span class="pill off">no</span>
											{/if}
										</td>
										<td class="col-p3 mono muted">
											v{m.agentVersion}{#if m.agentBuildVersion}
												· {m.agentBuildVersion}{/if}
										</td>
										<td>
											<div class="mini-bar {barTone(cpu)}">
												<span class="track"
													><span class="fill" style="width: {cpu ?? 0}%"></span></span
												>
												<span class="pct">{cpu == null ? '—' : `${Math.round(cpu)}%`}</span>
											</div>
										</td>
										<td>
											<div class="mini-bar {barTone(mem)}">
												<span class="track"
													><span class="fill" style="width: {mem ?? 0}%"></span></span
												>
												<span class="pct">{mem == null ? '—' : `${Math.round(mem)}%`}</span>
											</div>
										</td>
										<td class="mono">{m.strategies.length}</td>
										<td class="col-p3 mono muted">{formatHeartbeat(m.lastHeartbeat, now)}</td>
									</tr>
								{/each}
							</tbody>
						</table>
					</div>
				{/if}

				{#if isMobileRegionOpen(group.region, gi)}
					<div class="fleet-cards">
						{#each group.machines as m (machineId(m))}
							{@const id = machineId(m)}
							{@const st = machineStatus(m)}
							<a class="fleet-card" href="/machines/{id}">
								<div class="card-top">
									<strong class="mono">{id}</strong>
									<span class="pill {statusPillClass(st.kind)}">
										<span class="live-dot" class:off={st.kind === 'unreachable'}></span>
										{st.label}
									</span>
								</div>
								<div class="card-meta">
									<span>{m.reachable ? 'reachable' : 'unreachable'}</span>
									<span
										>{m.strategies.length} strateg{m.strategies.length === 1
											? 'y'
											: 'ies'}</span
									>
									{#if isBuildDrift(m)}
										<span class="pill drift">drift</span>
									{/if}
								</div>
							</a>
						{/each}
					</div>
				{/if}
			</section>
		{/each}
	{/if}
</section>

<style>
	.desktop-only {
		display: flex;
	}
	.mobile-only {
		display: none;
	}

	@media (max-width: 639px) {
		.desktop-only {
			display: none;
		}
		.mobile-only {
			display: flex;
		}
	}

	@media (min-width: 640px) {
		.fleet-cards {
			display: none !important;
		}
	}
</style>
