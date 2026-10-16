import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Loader2 } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { cn } from '@/lib/utils'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'

// 规则集「抓取代理」(iluobei/miaomiaowu#125):主控连不上 GitHub 时,GitHub 上的规则集改从加速站取。
// 「自动选择」= 抓取时按清单逐个试,用第一个取得到的;也可以固定用某一个,或自己填。

interface ProxyData {
  config: { enabled: boolean; server: string }
  presets: string[]
  last_good: string
}

interface ProbeResult {
  server: string
  ok: boolean
  ms: number
  error?: string
}

const AUTO = '__auto__'
const CUSTOM = '__custom__'
const KEY = ['rule-provider-proxy']

export function RuleProviderFetchProxyCard() {
  const queryClient = useQueryClient()
  const { data } = useQuery({
    queryKey: KEY,
    queryFn: async () =>
      (await api.get('/api/admin/rule-providers/proxy')).data as ProxyData,
  })
  // 自定义地址在点「保存」前只留在输入框里
  const [customOpen, setCustomOpen] = useState(false)
  const [custom, setCustom] = useState('')
  const [results, setResults] = useState<ProbeResult[] | null>(null)

  const server = data?.config.server ?? ''
  const isPreset = !!data && data.presets.includes(server)
  useEffect(() => {
    if (data && server && !isPreset) {
      setCustomOpen(true)
      setCustom(server)
    }
  }, [data, server, isPreset])

  const save = useMutation({
    mutationFn: async (cfg: { enabled: boolean; server: string }) =>
      (await api.put('/api/admin/rule-providers/proxy', cfg)).data as {
        config: ProxyData['config']
      },
    onSuccess: (res) => {
      queryClient.setQueryData(KEY, (old: ProxyData | undefined) =>
        old ? { ...old, config: res.config } : old
      )
      toast.success('抓取代理已保存')
    },
    onError: (e: { response?: { data?: { error?: string } } }) =>
      toast.error(e.response?.data?.error || '保存失败'),
  })

  const test = useMutation({
    mutationFn: async () =>
      (
        await api.post('/api/admin/rule-providers/proxy/test', {
          server: customOpen ? custom : '',
        })
      ).data as { results: ProbeResult[] },
    onSuccess: (res) => setResults(res.results),
    onError: () => toast.error('检测失败'),
  })

  if (!data) return null
  const enabled = data.config.enabled
  const selectValue = customOpen ? CUSTOM : server === '' ? AUTO : server

  const pick = (value: string) => {
    if (value === CUSTOM) {
      setCustomOpen(true)
      return
    }
    setCustomOpen(false)
    save.mutate({ enabled, server: value === AUTO ? '' : value })
  }

  return (
    <Card className='mt-6'>
      <CardContent className='space-y-3 py-4'>
        <div className='flex items-start justify-between gap-4'>
          <div className='min-w-0'>
            <div className='text-sm font-medium'>抓取代理</div>
            <p className='text-muted-foreground text-xs'>
              主控连不上 GitHub 时打开:远程规则集里 GitHub
              上的地址改从加速站抓取,其它来源照常直连。加速站是第三方,内容会经它们转一手。
            </p>
          </div>
          <Switch
            className='shrink-0'
            checked={enabled}
            disabled={save.isPending}
            onCheckedChange={(on) => save.mutate({ enabled: on, server })}
            aria-label='抓取代理'
          />
        </div>

        {enabled && (
          <>
            <div className='flex flex-wrap items-center gap-2'>
              <Select value={selectValue} onValueChange={pick}>
                <SelectTrigger
                  className='w-full sm:w-72'
                  aria-label='代理服务器'
                >
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value={AUTO}>自动选择可用的</SelectItem>
                  {data.presets.map((p) => (
                    <SelectItem key={p} value={p}>
                      {p}
                    </SelectItem>
                  ))}
                  <SelectItem value={CUSTOM}>自定义地址…</SelectItem>
                </SelectContent>
              </Select>
              <Button
                variant='outline'
                size='sm'
                disabled={test.isPending}
                onClick={() => test.mutate()}
              >
                {test.isPending && (
                  <Loader2 className='mr-1 size-4 animate-spin' />
                )}
                检测可用性
              </Button>
            </div>

            {customOpen && (
              <div className='flex flex-wrap items-center gap-2'>
                <Input
                  value={custom}
                  onChange={(e) => setCustom(e.target.value)}
                  placeholder='https://gh-proxy.com/'
                  aria-label='自定义代理服务器地址'
                  className='w-full sm:w-72'
                />
                <Button
                  size='sm'
                  disabled={!custom.trim() || save.isPending}
                  onClick={() =>
                    save.mutate({ enabled, server: custom.trim() })
                  }
                >
                  保存
                </Button>
              </div>
            )}

            {selectValue === AUTO && (
              <p className='text-muted-foreground text-xs'>
                {data.last_good
                  ? `上次抓取用的是 ${data.last_good}`
                  : '抓取时按清单逐个尝试,用第一个取得到的。'}
              </p>
            )}

            {results && (
              <div className='divide-y rounded-md border text-xs'>
                {results.map((r) => (
                  <div
                    key={r.server}
                    className='flex items-center gap-2 px-3 py-1.5'
                  >
                    <span
                      className={cn(
                        'inline-block size-2 shrink-0 rounded-full',
                        r.ok ? 'bg-emerald-500' : 'bg-muted-foreground/40'
                      )}
                    />
                    <span className='min-w-0 flex-1 truncate font-mono'>
                      {r.server}
                    </span>
                    <span className='text-muted-foreground shrink-0 tabular-nums'>
                      {r.ok ? `${r.ms} ms` : '不可用'}
                    </span>
                    {r.ok && r.server !== server && (
                      <Button
                        variant='ghost'
                        size='sm'
                        className='h-6 shrink-0 px-2 text-xs'
                        onClick={() => {
                          setCustomOpen(false)
                          save.mutate({ enabled, server: r.server })
                        }}
                      >
                        使用
                      </Button>
                    )}
                  </div>
                ))}
              </div>
            )}
          </>
        )}
      </CardContent>
    </Card>
  )
}
