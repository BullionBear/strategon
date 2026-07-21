import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { ArtifactRefSchema, ObjectMetaSchema } from '$lib/gen/strategyplatform/v1/common_pb';
import { MachineSchema, StrategyViewSchema } from '$lib/gen/strategyplatform/v1/control_service_pb';
import { MachineSpecSchema } from '$lib/gen/strategyplatform/v1/enrollment_pb';
import { DeployPhase } from '$lib/gen/strategyplatform/v1/status_pb';
import {
	compareAssignments,
	assignmentStatus,
	filterAssignments,
	flattenAssignments,
	regionsOf,
	statusCounts,
	statusPillClass,
	type AssignmentRow
} from './assignments';

function view(
	partial: Partial<{
		strategy: string;
		phase: DeployPhase;
		converged: boolean;
		stopped: boolean;
		cpuPercent: number;
		restartCount: number;
		desired: string;
		running: string;
	}>
) {
	return create(StrategyViewSchema, {
		strategy: partial.strategy ?? 'alpha',
		phase: partial.phase ?? DeployPhase.HEALTHY,
		converged: partial.converged ?? true,
		stopped: partial.stopped ?? false,
		cpuPercent: partial.cpuPercent ?? 0,
		restartCount: partial.restartCount ?? 0,
		desiredArtifact: create(ArtifactRefSchema, {
			name: 'a',
			version: partial.desired ?? 'v1',
			digest: 'sha256:aa'
		}),
		runningArtifact: create(ArtifactRefSchema, {
			name: 'a',
			version: partial.running ?? 'v1',
			digest: 'sha256:aa'
		})
	});
}

function machine(
	id: string,
	opts: { reachable?: boolean; region?: string; strategies?: ReturnType<typeof view>[] }
) {
	return create(MachineSchema, {
		metadata: create(ObjectMetaSchema, { uid: id, name: id }),
		reachable: opts.reachable ?? true,
		spec: create(MachineSpecSchema, { region: opts.region ?? '' }),
		strategies: opts.strategies ?? []
	});
}

function row(
	machineId: string,
	v: ReturnType<typeof view>,
	opts: { reachable?: boolean; region?: string } = {}
): AssignmentRow {
	return {
		machineId,
		region: opts.region ?? 'Unassigned',
		reachable: opts.reachable ?? true,
		view: v
	};
}

describe('flattenAssignments', () => {
	it('emits one row per strategy', () => {
		const machines = [
			machine('m1', {
				region: 'tw',
				strategies: [view({ strategy: 'a' }), view({ strategy: 'b' })]
			}),
			machine('m2', { strategies: [view({ strategy: 'c' })] })
		];
		const rows = flattenAssignments(machines);
		expect(rows.map((r) => `${r.machineId}/${r.view.strategy}`)).toEqual([
			'm1/a',
			'm1/b',
			'm2/c'
		]);
		expect(rows[0].region).toBe('tw');
		expect(rows[2].region).toBe('Unassigned');
	});
});

describe('assignmentStatus', () => {
	it('ranks worst-first', () => {
		const kinds = [
			row('m', view({}), { reachable: false }),
			row('m', view({ phase: DeployPhase.FAILED, converged: false })),
			row('m', view({ phase: DeployPhase.DOWNLOADING, converged: false })),
			row('m', view({ stopped: true, converged: true, phase: DeployPhase.STOPPED })),
			row('m', view({ converged: true, phase: DeployPhase.HEALTHY }))
		].map((d) => assignmentStatus(d));

		expect(kinds.map((k) => k.kind)).toEqual([
			'unreachable',
			'failed',
			'diverging',
			'stopped',
			'healthy'
		]);
		expect(kinds.map((k) => k.rank)).toEqual([0, 1, 2, 3, 4]);
	});

	it('prefers failed over stopped', () => {
		const st = assignmentStatus(
			row('m', view({ phase: DeployPhase.FAILED, stopped: true, converged: false }))
		);
		expect(st.kind).toBe('failed');
	});

	it('maps pill classes', () => {
		expect(statusPillClass('unreachable')).toBe('bad');
		expect(statusPillClass('failed')).toBe('bad');
		expect(statusPillClass('diverging')).toBe('lag');
		expect(statusPillClass('stopped')).toBe('off');
		expect(statusPillClass('healthy')).toBe('ok');
	});
});

describe('filterAssignments', () => {
	const rows = [
		row('host-a', view({ strategy: 'alpha' }), { region: 'tw' }),
		row('host-b', view({ strategy: 'beta', phase: DeployPhase.FAILED, converged: false }), {
			region: 'jp'
		}),
		row('host-c', view({ strategy: 'gamma', stopped: true, phase: DeployPhase.STOPPED }), {
			region: 'tw',
			reachable: false
		})
	];

	it('matches text on strategy or machine', () => {
		expect(filterAssignments(rows, { text: 'alp', statuses: new Set(), region: '' })).toHaveLength(
			1
		);
		expect(
			filterAssignments(rows, { text: 'HOST-B', statuses: new Set(), region: '' })
		).toHaveLength(1);
	});

	it('filters by status membership', () => {
		const failed = filterAssignments(rows, {
			text: '',
			statuses: new Set(['failed']),
			region: ''
		});
		expect(failed.map((r) => r.view.strategy)).toEqual(['beta']);
	});

	it('filters by region', () => {
		const tw = filterAssignments(rows, { text: '', statuses: new Set(), region: 'tw' });
		expect(tw.map((r) => r.view.strategy).sort()).toEqual(['alpha', 'gamma']);
	});

	it('combines filters', () => {
		const out = filterAssignments(rows, {
			text: 'host',
			statuses: new Set(['unreachable']),
			region: 'tw'
		});
		expect(out.map((r) => r.view.strategy)).toEqual(['gamma']);
	});
});

describe('compareAssignments', () => {
	const a = row('m1', view({ strategy: 'zeta', cpuPercent: 10, restartCount: 1, desired: 'v2' }), {
		region: 'b'
	});
	const b = row(
		'm2',
		view({
			strategy: 'alpha',
			cpuPercent: 50,
			restartCount: 5,
			desired: 'v1',
			phase: DeployPhase.FAILED,
			converged: false
		}),
		{ region: 'a' }
	);

	it('sorts status ascending (worst first)', () => {
		const sorted = [a, b].sort((x, y) => compareAssignments(x, y, 'status', 'asc'));
		expect(sorted[0].view.strategy).toBe('alpha'); // failed before healthy
	});

	it('sorts strategy ascending with machine tiebreak', () => {
		const same = [
			row('m2', view({ strategy: 'x' })),
			row('m1', view({ strategy: 'x' }))
		].sort((x, y) => compareAssignments(x, y, 'strategy', 'asc'));
		expect(same.map((r) => r.machineId)).toEqual(['m1', 'm2']);
	});

	it('sorts numeric keys descending by default intent', () => {
		const byCpu = [a, b].sort((x, y) => compareAssignments(x, y, 'cpu', 'desc'));
		expect(byCpu[0].view.cpuPercent).toBe(50);
		const byRestarts = [a, b].sort((x, y) => compareAssignments(x, y, 'restarts', 'desc'));
		expect(byRestarts[0].view.restartCount).toBe(5);
	});

	it('sorts version and region', () => {
		const byVer = [a, b].sort((x, y) => compareAssignments(x, y, 'version', 'asc'));
		expect(byVer[0].view.desiredArtifact?.version).toBe('v1');
		const byReg = [a, b].sort((x, y) => compareAssignments(x, y, 'region', 'asc'));
		expect(byReg[0].region).toBe('a');
	});
});

describe('statusCounts / regionsOf', () => {
	it('counts and lists regions', () => {
		const rows = [
			row('m1', view({}), { region: 'tw' }),
			row('m2', view({ phase: DeployPhase.FAILED, converged: false }), { region: 'jp' }),
			row('m3', view({}), { region: 'tw' })
		];
		expect(statusCounts(rows)).toEqual({
			unreachable: 0,
			failed: 1,
			diverging: 0,
			stopped: 0,
			healthy: 2
		});
		expect(regionsOf(rows)).toEqual(['jp', 'tw']);
	});
});
