import type { Machine, StrategyView } from '$lib/gen/strategyplatform/v1/control_service_pb';
import { isFailPhase, phaseLabel } from '$lib/phases';
import { heartbeatMs, machineId, machineRegion } from '$lib/fleet';

/** One flattened (machine, strategy) assignment row. */
export type AssignmentRow = {
	machineId: string;
	region: string;
	reachable: boolean;
	view: StrategyView;
};

export type AssignmentStatusKind =
	| 'unreachable'
	| 'failed'
	| 'diverging'
	| 'stopped'
	| 'healthy';

export type AssignmentStatus = {
	kind: AssignmentStatusKind;
	label: string;
	/** Lower = worse; default sort puts anomalies first. */
	rank: number;
	/** Phase label for the Status column (empty when not useful). */
	phase: string;
};

/** All status kinds in worst-first display order (for chips). */
export const STATUS_KINDS: AssignmentStatusKind[] = [
	'failed',
	'diverging',
	'unreachable',
	'stopped',
	'healthy'
];

export function flattenAssignments(machines: Machine[]): AssignmentRow[] {
	const out: AssignmentRow[] = [];
	for (const m of machines) {
		const id = machineId(m);
		const region = machineRegion(m);
		for (const view of m.strategies) {
			out.push({ machineId: id, region, reachable: m.reachable, view });
		}
	}
	return out;
}

export function assignmentKey(d: AssignmentRow): string {
	return `${d.machineId}\0${d.view.strategy}`;
}

/**
 * Mirrors machine-detail badge priority so the list and detail never disagree:
 * unreachable → failed → diverging → stopped → healthy.
 */
export function assignmentStatus(d: AssignmentRow): AssignmentStatus {
	const phase = phaseLabel(d.view.phase);
	if (!d.reachable) {
		return { kind: 'unreachable', label: 'unreachable', rank: 0, phase };
	}
	if (isFailPhase(d.view.phase)) {
		return { kind: 'failed', label: 'failed', rank: 1, phase };
	}
	if (!d.view.converged) {
		return { kind: 'diverging', label: 'diverging', rank: 2, phase };
	}
	if (d.view.stopped) {
		return { kind: 'stopped', label: 'stopped', rank: 3, phase };
	}
	return { kind: 'healthy', label: 'healthy', rank: 4, phase };
}

export function statusPillClass(kind: AssignmentStatusKind): string {
	switch (kind) {
		case 'unreachable':
		case 'failed':
			return 'bad';
		case 'diverging':
			return 'lag';
		case 'stopped':
			return 'off';
		default:
			return 'ok';
	}
}

export type StatusCounts = Record<AssignmentStatusKind, number>;

export function statusCounts(rows: AssignmentRow[]): StatusCounts {
	const counts: StatusCounts = {
		unreachable: 0,
		failed: 0,
		diverging: 0,
		stopped: 0,
		healthy: 0
	};
	for (const d of rows) {
		counts[assignmentStatus(d).kind]++;
	}
	return counts;
}

export type AssignmentFilter = {
	/** Case-insensitive substring on strategy or machine id. */
	text: string;
	/** Empty = all; otherwise membership. */
	statuses: ReadonlySet<AssignmentStatusKind>;
	/** Exact region, or '' / 'all' for any. */
	region: string;
};

export function filterAssignments(rows: AssignmentRow[], f: AssignmentFilter): AssignmentRow[] {
	const q = f.text.trim().toLowerCase();
	const region = f.region.trim();
	const allRegions = !region || region === 'all';
	const allStatuses = f.statuses.size === 0;
	return rows.filter((d) => {
		if (!allStatuses && !f.statuses.has(assignmentStatus(d).kind)) return false;
		if (!allRegions && d.region !== region) return false;
		if (!q) return true;
		return (
			d.view.strategy.toLowerCase().includes(q) || d.machineId.toLowerCase().includes(q)
		);
	});
}

export function regionsOf(rows: AssignmentRow[]): string[] {
	const set = new Set<string>();
	for (const d of rows) set.add(d.region);
	return [...set].sort((a, b) => {
		if (a === 'Unassigned') return 1;
		if (b === 'Unassigned') return -1;
		return a.localeCompare(b);
	});
}

export type AssignmentSortKey =
	| 'strategy'
	| 'machine'
	| 'region'
	| 'status'
	| 'version'
	| 'cpu'
	| 'restarts'
	| 'deployed';

export type SortDir = 'asc' | 'desc';

function cmpNum(a: number, b: number): number {
	return a === b ? 0 : a < b ? -1 : 1;
}

function cmpStr(a: string, b: string): number {
	return a.localeCompare(b, undefined, { sensitivity: 'base' });
}

function tiebreak(a: AssignmentRow, b: AssignmentRow): number {
	const s = cmpStr(a.view.strategy, b.view.strategy);
	if (s !== 0) return s;
	return cmpStr(a.machineId, b.machineId);
}

export function compareAssignments(
	a: AssignmentRow,
	b: AssignmentRow,
	key: AssignmentSortKey,
	dir: SortDir
): number {
	const mul = dir === 'asc' ? 1 : -1;
	let r = 0;
	switch (key) {
		case 'strategy':
			r = cmpStr(a.view.strategy, b.view.strategy);
			break;
		case 'machine':
			r = cmpStr(a.machineId, b.machineId);
			break;
		case 'region':
			r = cmpStr(a.region, b.region);
			break;
		case 'status':
			r = cmpNum(assignmentStatus(a).rank, assignmentStatus(b).rank);
			break;
		case 'version':
			r = cmpStr(
				a.view.desiredArtifact?.version || '',
				b.view.desiredArtifact?.version || ''
			);
			break;
		case 'cpu':
			r = cmpNum(a.view.cpuPercent || -1, b.view.cpuPercent || -1);
			break;
		case 'restarts':
			r = cmpNum(a.view.restartCount, b.view.restartCount);
			break;
		case 'deployed':
			r = cmpNum(heartbeatMs(a.view.deployedAt), heartbeatMs(b.view.deployedAt));
			break;
	}
	if (r === 0) r = tiebreak(a, b);
	return r * mul;
}

/** Default sort direction when a column is first selected. */
export function defaultSortDir(key: AssignmentSortKey): SortDir {
	switch (key) {
		case 'strategy':
		case 'machine':
		case 'region':
		case 'status':
		case 'version':
			return 'asc';
		default:
			return 'desc';
	}
}
