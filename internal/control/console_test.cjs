const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');

const {readStream, batchItems} = vm.runInNewContext(
  fs.readFileSync(path.join(__dirname, 'console', 'console.js'), 'utf8') +
    '\n;({readStream, batchItems})',
  {TextDecoder},
);

test('SSE handles byte splits, CRLF, and stops at DONE', async () => {
  const data = Buffer.from('data: {"choices":[{"delta":{"content":"你好"}}]}\r\n\r\n' +
    'data: {"choices":[{"finish_reason":"length"}]}\r\n\r\ndata: [DONE]\r\n\r\n');
  const body = new ReadableStream({
    start(controller) {
      for (const byte of data) controller.enqueue(Uint8Array.of(byte));
    },
  });
  let text = '';
  const reason = await readStream(body, content => { text += content; });
  assert.equal(reason, 'length');
  assert.equal(text, '你好');
  assert.equal(body.locked, false);
});

test('a truncated stream cannot be mistaken for a success', async () => {
  const body = new ReadableStream({
    start(controller) {
      controller.enqueue(Buffer.from('data: {"choices":[{"delta":{"content":"部分"}}]}\n\n'));
      controller.close();
    },
  });
  await assert.rejects(readStream(body, () => {}), /中断/);
});

test('batch submits nonempty lines in order and enforces the item limit', () => {
  const items = batchItems('第一条\r\n\r\n 第二条 ', 'gpu', 8);
  assert.equal(JSON.stringify(items), JSON.stringify([
    {model: 'gpu', messages: [{role: 'user', content: '第一条'}], max_tokens: 8, stream: false},
    {model: 'gpu', messages: [{role: 'user', content: '第二条'}], max_tokens: 8, stream: false},
  ]));
  assert.throws(() => batchItems('过多\n'.repeat(21), 'gpu', 8), /20/);
});
