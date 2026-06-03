<?php

declare(strict_types=1);

require __DIR__ . '/vendor/autoload.php';

use Nyholm\Psr7\Factory\Psr17Factory;
use Spiral\RoadRunner;
use Spiral\RoadRunner\Http\PSR7Worker;

$worker     = RoadRunner\Worker::create();
$psrFactory = new Psr17Factory();
$psr7       = new PSR7Worker($worker, $psrFactory, $psrFactory, $psrFactory);

$counter = 0;

while (($req = $psr7->waitRequest()) !== null) {
    try {
        $counter++;
        $body = sprintf(
            "hello world #%d pid=%d path=%s method=%s\n",
            $counter,
            getmypid(),
            $req->getUri()->getPath(),
            $req->getMethod(),
        );

        $resp = $psrFactory->createResponse(200)
            ->withHeader('Content-Type', 'text/plain')
            ->withBody($psrFactory->createStream($body));

        $psr7->respond($resp);
    } catch (\Throwable $e) {
        $psr7->getWorker()->error((string) $e);
    }
}
