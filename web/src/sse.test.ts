import { describe, expect, it } from 'vitest'
import { SSEParser } from './sse'

describe('SSEParser', () => {
  it('reads whole messages', () => {
    const p = new SSEParser()
    expect(p.push('event: hello\ndata: {"vms":[]}\n\nevent: vm\ndata: {"id":"a"}\n\n')).toEqual([
      { event: 'hello', data: '{"vms":[]}' },
      { event: 'vm', data: '{"id":"a"}' },
    ])
  })

  it('handles chunks cut anywhere', () => {
    const p = new SSEParser()
    const text = 'event: invoke\ndata: {"function":"hello"}\n\n'
    const got = []
    for (const ch of text) got.push(...p.push(ch))
    expect(got).toEqual([{ event: 'invoke', data: '{"function":"hello"}' }])
  })

  it('skips comments, joins data lines, accepts CRLF and no space', () => {
    const p = new SSEParser()
    expect(p.push(': ping\n\n')).toEqual([])
    expect(p.push('data:a\r\ndata: b\r\n\r\n')).toEqual([{ event: 'message', data: 'a\nb' }])
  })

  it('keeps an unfinished message for the next chunk', () => {
    const p = new SSEParser()
    expect(p.push('event: vm\ndata: {"id":')).toEqual([])
    expect(p.push('"x"}\n\n')).toEqual([{ event: 'vm', data: '{"id":"x"}' }])
  })
})
