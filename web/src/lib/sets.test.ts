import { create } from '@bufbuild/protobuf';
import { describe, expect, it } from 'vitest';
import { ObjectMetaSchema } from '$lib/gen/strategyplatform/v1/common_pb';
import {
	AssignmentSetSchema,
	AssignmentSetSpecSchema,
	AssignmentSetStatusSchema
} from '$lib/gen/strategyplatform/v1/assignmentset_pb';
import { setGenerationLag, setPhaseClass, setStrategy, memberPhaseLabel } from './sets';

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
