const {
  checkFunctionResponse,
  checkInClusterEventDelivery,
  sendLegacyEventAndCheckResponse,
  sendCloudEventStructuredModeAndCheckResponse,
  sendCloudEventBinaryModeAndCheckResponse,
  deleteMockTestFixture,
  ensureCommerceMockWithCompassTestFixture,
} = require('../../test/fixtures/commerce-mock');
const {
  AuditLogCreds,
  AuditLogClient,
  checkAuditLogs,
} = require('../../audit-log');
const {debug} = require('../../utils');
const {director} = require('../provision/provision-skr');

const AWS_PLAN_ID = '361c511f-f939-4621-b228-d0fb79a1fe15';

// const auditLogsThreshold = 4;
const testTimeout = 1000 * 60 * 30; // 30m

// prepares all the resources required for commerce mock to be executed;
// runs the actual tests and checks the audit logs in case of AWS plan
function commerceMockTest(options) {
  context('CommerceMock Test', async function() {
    this.timeout(testTimeout);
    if (options === undefined) {
      throw new Error('Empty configuration given');
    }
    commerceMockTestPreparation(options);
    commerceMockTests(options.testNS);
    commerceMockCleanup(options.testNS);

    it('Check audit logs for AWS', async function() {
      if (process.env.KEB_PLAN_ID === AWS_PLAN_ID) {
        checkAuditLogsForAWS();
      } else {
        debug('Skipping step for non-AWS plan');
      }
    });
  });
}


function commerceMockTestPreparation(options) {
  it('CommerceMock test fixture should be ready', async function() {
    await ensureCommerceMockWithCompassTestFixture(
        director,
        options.appName,
        options.scenarioName,
        'mocks',
        options.testNS,
        true,
    );
  });
}

// executes the actual commerce mock tests
function commerceMockTests(testNamespace) {
  it('in-cluster event should be delivered (structured and binary mode)', async function() {
    await checkInClusterEventDelivery(testNamespace);
  });

  it('function should be reachable through secured API Rule', async function() {
    await checkFunctionResponse(testNamespace);
  });

  it('order.created.v1 legacy event should trigger the lastorder function', async function() {
    await sendLegacyEventAndCheckResponse();
  });

  it('order.created.v1 cloud event in structured mode should trigger the lastorder function', async function() {
    await sendCloudEventStructuredModeAndCheckResponse();
  });

  it('order.created.v1 cloud event in binary mode should trigger the lastorder function', async function() {
    await sendCloudEventBinaryModeAndCheckResponse();
  });
}

function commerceMockCleanup(testNamespace) {
  it('CommerceMock test fixture should be deleted', async function() {
    await deleteMockTestFixture('mocks', testNamespace);
  });
}

function checkAuditLogsForAWS() {
  it('Check audit logs', async function() {
    const auditLogs = new AuditLogClient(AuditLogCreds.fromEnv());
    await checkAuditLogs(auditLogs, null);
  });

  // // TODO: Enable checkAuditEventsThreshold again when fix is ready by Andreas Thaler
  // it('Amount of audit events must not exceed a certain threshold', async function() {
  //   await checkAuditEventsThreshold(auditLogsThreshold);
  // });
}

module.exports = {
  commerceMockTestPreparation,
  commerceMockTest,
};
