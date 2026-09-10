import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { ObjectMetaSchema } from '$lib/gen/strategyplatform/v1/common_pb';
import {
	AssignmentSetSchema,
	AssignmentSetSpecSchema,
	AssignmentSetStatusSchema,
	MemberStatusSchema,
	SetMemberSchema
} from '$lib/gen/strategyplatform/v1/assignmentset_pb';
import {
	memberPhaseLabel,
	memberRowKey,
	memberStatusOf,
	setGenerationLag,
	setPhaseClass,
	setStrategy
} from './sets';

describe('setPhaseClass', () => {
	it('maps controller phases', () => {
		expect(setPhaseClass('Ready')).toBe('ok');
		expect(setPhaseClass('Failed')).toBe('bad');
		expect(setPhaseClass('Degraded')).toBe('bad');
		expect(setPhaseClass('Rolling')).toBe('lag');
		expect(setPhaseClass('Deleting')).toBe('off');
	});
});

describe('cluster helpers', () => {
	it('detects generation lag and default strategy', () => {
		const c = create(AssignmentSetSchema, {
			metadata: create(ObjectMetaSchema, { name: 'trading', generation: 2n }),
			spec: create(AssignmentSetSpecSchema, {}),
			status: create(AssignmentSetStatusSchema, { observedGeneration: 1n, phase: 'Rolling' })
		});
		expect(setGenerationLag(c)).toBe(true);
		expect(setStrategy(c)).toBe('nats');
	});
});

describe('memberPhaseLabel', () => {
	it('pretty-prints proto enum names', () => {
		expect(memberPhaseLabel('DEPLOY_PHASE_HEALTHY')).toBe('Healthy');
		expect(memberPhaseLabel('DEPLOY_PHASE_HEALTH_CHECKING')).toBe('Health Checking');
		expect(memberPhaseLabel('HEALTHY')).toBe('Healthy');
		expect(memberPhaseLabel('')).toBe('—');
	});
});

describe('member row identity', () => {
	it('keys same-host members separately and matches status by machine+name', () => {
		expect(memberRowKey('m1', 'nats-a')).not.toBe(memberRowKey('m1', 'nats-b'));
		const c = create(AssignmentSetSchema, {
			spec: create(AssignmentSetSpecSchema, {
				members: [
					create(SetMemberSchema, { machine: 'm1', name: 'nats-a' }),
					create(SetMemberSchema, { machine: 'm1', name: 'nats-b' })
				]
			}),
			status: create(AssignmentSetStatusSchema, {
				members: [
					create(MemberStatusSchema, {
						machine: 'm1',
						name: 'nats-a',
						ready: true,
						phase: 'DEPLOY_PHASE_HEALTHY',
						converged: true
					}),
					create(MemberStatusSchema, {
						machine: 'm1',
						name: 'nats-b',
						ready: false,
						phase: 'DEPLOY_PHASE_DEPLOYING',
						converged: false
					})
				]
			})
		});
		expect(memberStatusOf(c, 'm1', 'nats-a')?.ready).toBe(true);
		expect(memberStatusOf(c, 'm1', 'nats-b')?.phase).toBe('DEPLOY_PHASE_DEPLOYING');
	});
});
