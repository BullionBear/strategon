<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { client } from '$lib/api';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { barTone, formatClock, formatUptime } from '$lib/fleet';
	import {
		STATUS_KINDS,
		compareAssignments,
		defaultSortDir,
		assignmentKey,
		assignmentStatus,
		filterAssignments,
		flattenAssignments,
		regionsOf,
		statusCounts,
		statusPillClass,
		type AssignmentFilter,
		type AssignmentRow,
		type AssignmentSortKey,
		type AssignmentStatusKind,
		type SortDir
	} from '$lib/assignments';

	let machines = $state<Machine[]>([]);
	let error = $state('');
	let loading = $state(true);
	let now = $state(Date.now());

	let text = $state('');
	let region = $state('all');
	let statusFilter = $state<Set<AssignmentStatusKind>>(new Set());
	let sortKey = $state<AssignmentSortKey>('status');
	let sortDir = $state<SortDir>('asc');

	const allRows = $derived(flattenAssignments(machines));
	const counts = $derived(statusCounts(allRows));
	const regions = $derived(regionsOf(allRows));

	const filter = $derived<AssignmentFilter>({
		text,
		statuses: statusFilter,
		region
	});

	const filtered = $derived(filterAssignments(allRows, filter));
	const rows = $derived(
		[...filtered].sort((a, b) => compareAssignments(a, b, sortKey, sortDir))
	);

	function toggleStatus(kind: AssignmentStatusKind) {
		const next = new Set(statusFilter);
		if (next.has(kind)) next.delete(kind);
		else next.add(kind);
		statusFilter = next;
	}

	function clearFilters() {
		text = '';
		region = 'all';
		statusFilter = new Set();
	}

	const hasFilters = $derived(
		text.trim() !== '' || region !== 'all' || statusFilter.size > 0
	);

	function setSort(key: AssignmentSortKey) {
		if (sortKey === key) {
			sortDir = sortDir === 'asc' ? 'desc' : 'asc';
		} else {
			sortKey = key;
			sortDir = defaultSortDir(key);
		}
	}

	function sortMark(key: AssignmentSortKey): string {
		if (sortKey !== key) return '↕';
		return sortDir === 'asc' ? '↑' : '↓';
	}

	function goRow(d: AssignmentRow) {
		goto(`/machines/${d.machineId}/${d.view.strategy}`);
	}

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
		const poll = setInterval(refresh, 2000);
		const tick = setInterval(() => {
			now = Date.now();
		}, 1000);
		return () => {
			clearInterval(poll);
			clearInterval(tick);
		};
	});
</script>

<section class="fade-in">
	<h1>Assignments</h1>
	<p class="muted">Every (machine, strategy) assignment across the fleet. Polls every 2s.</p>

	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}
	{#if loading}
		<p class="muted" style="margin-top:1rem">Loading…</p>
	{:else if allRows.length === 0}
		<p class="muted" style="margin-top:1.25rem">
			No assignments yet. Assign a strategy from <a href="/deploy">Deploy</a>.
		</p>
	{:else}
		<div class="chips" role="group" aria-label="Filter by status">
			{#each STATUS_KINDS as kind}
				<button
					type="button"
					class="chip"
					class:active={statusFilter.has(kind)}
					class:zero={counts[kind] === 0}
					onclick={() => toggleStatus(kind)}
				>
					<span class="pill {statusPillClass(kind)}">{kind}</span>
					<span class="chip-count mono">{counts[kind]}</span>
				</button>
			{/each}
		</div>

		<div class="controls">
			<input
				type="search"
				placeholder="Filter strategy or machine…"
				bind:value={text}
				aria-label="Filter by strategy or machine"
			/>
			<select bind:value={region} aria-label="Filter by region">
				<option value="all">All regions</option>
				{#each regions as r}
					<option value={r}>{r}</option>
				{/each}
			</select>
			{#if hasFilters}
				<button type="button" class="btn secondary" onclick={clearFilters}>Clear</button>
			{/if}
			<span class="result-count mono muted">{rows.length} / {allRows.length}</span>
		</div>

		{#if rows.length === 0}
			<p class="muted" style="margin-top:1rem">No assignments match the current filters.</p>
		{:else}
			<div class="fleet-table-wrap">
				<table class="fleet-table">
					<thead>
						<tr>
							<th
								class="sortable"
								class:sorted={sortKey === 'strategy'}
								onclick={() => setSort('strategy')}
							>
								Strategy <span class="sort-ind">{sortMark('strategy')}</span>
							</th>
							<th
								class="sortable"
								class:sorted={sortKey === 'machine'}
								onclick={() => setSort('machine')}
							>
								Machine <span class="sort-ind">{sortMark('machine')}</span>
							</th>
							<th
								class="sortable col-p3"
								class:sorted={sortKey === 'region'}
								onclick={() => setSort('region')}
							>
								Region <span class="sort-ind">{sortMark('region')}</span>
							</th>
							<th
								class="sortable"
								class:sorted={sortKey === 'status'}
								onclick={() => setSort('status')}
							>
								Status <span class="sort-ind">{sortMark('status')}</span>
							</th>
							<th
								class="sortable col-p3"
								class:sorted={sortKey === 'version'}
								onclick={() => setSort('version')}
							>
								Desired → Actual <span class="sort-ind">{sortMark('version')}</span>
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
								class:sorted={sortKey === 'restarts'}
								onclick={() => setSort('restarts')}
							>
								Restarts <span class="sort-ind">{sortMark('restarts')}</span>
							</th>
							<th
								class="sortable col-p3"
								class:sorted={sortKey === 'deployed'}
								onclick={() => setSort('deployed')}
							>
								Deployed <span class="sort-ind">{sortMark('deployed')}</span>
							</th>
						</tr>
					</thead>
					<tbody>
						{#each rows as d (assignmentKey(d))}
							{@const st = assignmentStatus(d)}
							{@const cpu = d.view.cpuPercent}
							{@const href = `/machines/${d.machineId}/${d.view.strategy}`}
							<tr
								onclick={() => goRow(d)}
								onkeydown={(e) => {
									if (e.key === 'Enter' || e.key === ' ') {
										e.preventDefault();
										goRow(d);
									}
								}}
								tabindex="0"
								role="link"
							>
								<td>
									<a class="row-link mono" {href} onclick={(e) => e.stopPropagation()}
										>{d.view.strategy}</a
									>
								</td>
								<td>
									<a
										class="row-link mono muted"
										href="/machines/{d.machineId}"
										onclick={(e) => e.stopPropagation()}>{d.machineId}</a
									>
								</td>
								<td class="col-p3 muted">{d.region}</td>
								<td>
									<span class="pill {statusPillClass(st.kind)}">
										<span class="live-dot" class:off={st.kind === 'unreachable' || st.kind === 'stopped'}
										></span>
										{st.label}
									</span>
									{#if st.phase && st.phase !== '—' && st.kind !== 'healthy' && st.kind !== 'stopped'}
										<span class="phase mono muted">{st.phase}</span>
									{/if}
								</td>
								<td class="col-p3 mono">
									<span class:mismatch={!d.view.converged}
										>{d.view.desiredArtifact?.version || '—'} → {d.view.runningArtifact
											?.version || '—'}</span
									>
								</td>
								<td>
									<div class="mini-bar {barTone(Number.isFinite(cpu) ? cpu : null)}">
										<span class="track"
											><span class="fill" style="width: {Math.min(cpu || 0, 100)}%"></span
											></span
										>
										<span class="pct">{Math.round(cpu || 0)}%</span>
									</div>
								</td>
								<td class="mono">{d.view.restartCount}</td>
								<td class="col-p3 mono muted">
									{#if d.view.deployedAt}
										{formatClock(d.view.deployedAt)}
										{#if d.view.startedAt && !d.view.stopped}
											· up {formatUptime(d.view.startedAt, now)}{/if}
									{:else}
										—
									{/if}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>

			<div class="assignment-cards">
				{#each rows as d (assignmentKey(d))}
					{@const st = assignmentStatus(d)}
					<a class="fleet-card" href="/machines/{d.machineId}/{d.view.strategy}">
						<div class="card-top">
							<strong class="mono">{d.view.strategy}</strong>
							<span class="pill {statusPillClass(st.kind)}">
								<span class="live-dot" class:off={st.kind === 'unreachable' || st.kind === 'stopped'}
								></span>
								{st.label}
							</span>
						</div>
						<div class="card-meta">
							<span class="mono">{d.machineId}</span>
							<span>{d.region}</span>
							<span class="mono"
								>{d.view.desiredArtifact?.version || '—'} → {d.view.runningArtifact?.version ||
									'—'}</span
							>
						</div>
					</a>
				{/each}
			</div>
		{/if}
	{/if}
</section>

<style>
	.chips {
		display: flex;
		flex-wrap: wrap;
		gap: 0.45rem;
		margin-top: 1.25rem;
	}
	.chip {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		padding: 0.2rem 0.35rem 0.2rem 0.2rem;
		border: 1px solid var(--line);
		border-radius: 999px;
		background: var(--surface, #fff);
		cursor: pointer;
		font: inherit;
		color: inherit;
	}
	.chip:hover {
		border-color: var(--ink-muted);
	}
	.chip.active {
		border-color: var(--ink);
		box-shadow: 0 0 0 1px var(--ink);
	}
	.chip.zero {
		opacity: 0.45;
	}
	.chip-count {
		font-size: 0.78rem;
		padding-right: 0.35rem;
		color: var(--ink-muted);
	}
	.chip.active .chip-count {
		color: var(--ink);
		font-weight: 600;
	}

	.controls {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.65rem;
		margin-top: 1rem;
	}
	.controls input[type='search'] {
		flex: 1 1 14rem;
		min-width: 10rem;
		padding: 0.45rem 0.65rem;
		border: 1px solid var(--line);
		border-radius: 6px;
		font: inherit;
		background: var(--surface, #fff);
		color: inherit;
	}
	.controls select {
		padding: 0.45rem 0.65rem;
		border: 1px solid var(--line);
		border-radius: 6px;
		font: inherit;
		background: var(--surface, #fff);
		color: inherit;
	}
	.result-count {
		margin-left: auto;
		font-size: 0.85rem;
	}

	.phase {
		margin-left: 0.35rem;
		font-size: 0.75rem;
	}
	.mismatch {
		color: var(--lag);
	}

	.assignment-cards {
		display: none;
		flex-direction: column;
		gap: 0.65rem;
		margin-top: 1rem;
	}

	@media (max-width: 639px) {
		.fleet-table-wrap {
			display: none;
		}
		.assignment-cards {
			display: flex;
		}
		.result-count {
			margin-left: 0;
			width: 100%;
		}
	}
</style>
