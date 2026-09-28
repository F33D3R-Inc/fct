package selfhost

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The crate-by-crate coverage table of the fabric self-hosting: one row per
// Rust #[test] in fabric/crates (and one per Rust module that has none),
// naming the .fct port that carries its behaviour and the Go test that
// asserts the port against the crate. The table is checked against the
// tree, not trusted: every #[test] fabric/crates holds must have a row,
// every row's test must still exist where the row says, the port file must
// exist, and the Go test must be declared in this package. A row whose port
// is "" is a known gap and skips as "not ported", so the gap is one
// `go test -run TestFabricCoverage -v | grep SKIP` away.
type fabricCoverageRow struct {
	crate  string // the crate under fabric/crates
	file   string // the Rust file, relative to the crate
	test   string // the #[test] fn, or "" for a module with no tests
	port   string // the .fct file in selfhost, or "" when not ported
	goTest string // the Go test here that checks the port against the crate; for an unported row, why
}

var fabricCoverage = []fabricCoverageRow{
	// fabric-cli
	{"fabric-cli", "src/args.rs", "a_non_numeric_liveness_flag_errors", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "daemon_commands_parse", "fabric_cli.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/args.rs", "daemon_commands_refuse_what_they_cannot_mean", "fabric_cli.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/daemon.rs", "a_response_is_read_by_its_framing", "fabric_cli_daemon.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/daemon.rs", "only_a_plain_http_authority_is_an_operator_port", "fabric_cli_daemon.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/daemon.rs", "placements_render_one_row_per_cell", "fabric_cli_daemon.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/daemon.rs", "the_token_is_never_optional_and_the_port_never_guessed", "fabric_cli_daemon.fct", "TestFabricCliDaemon"},
	{"fabric-cli", "src/args.rs", "bare_subcommand_defaults", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "every_subcommand_parses", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "help_and_version_flags", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "input_and_json_flags", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "input_short_flag_and_equals_form", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "liveness_flags_parse_in_both_forms", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "missing_input_value_errors", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "no_args_is_help", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "stdin_dash_is_accepted_as_input", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "subcommand_help", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "unexpected_positional_errors", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "unknown_command_errors", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/args.rs", "unknown_flag_errors", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/main.rs", "", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "action_labels", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "empty_renderers_are_explicit", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "nodes_empty_and_populated", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "predict_empty_and_populated", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "pressure_labels", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "status_json_roundtrips", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "topology_human_and_json", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/render.rs", "validation_valid_and_invalid", "fabric_cli_render.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "empty_array_is_valid_and_empty", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "example_session_populates_runtime", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "malformed_json_is_parse_error", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "missing_file_is_io_error", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "no_path_yields_empty_runtime", "fabric_cli.fct", "TestFabricCli"},
	{"fabric-cli", "src/session.rs", "unknown_message_variant_is_parse_error", "fabric_cli.fct", "TestFabricCli"},
	// fabric-controller
	{"fabric-controller", "src/controller.rs", "a_decision_computed_against_an_older_fleet_is_refused", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "a_plan_that_would_drop_the_last_copy_is_refused", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "a_saturated_destination_is_refused_even_with_headroom", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "a_stale_decision_is_refused_with_its_evidence", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "an_action_that_helped_nothing_is_reported_as_such", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "an_executed_action_is_not_complete_until_it_is_measured", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "an_operator_request_is_held_to_every_safety_check", "fabric_controller_core.fct", "TestFabricControllerCoreOperatorRequest"},
	{"fabric-controller", "src/controller.rs", "an_operator_request_is_not_held_to_the_models_threshold", "fabric_controller_core.fct", "TestFabricControllerCoreOperatorRequest"},
	{"fabric-controller", "src/controller.rs", "an_optimizer_envelope_serializes_as_before", "", "DecisionEnvelope's serde shape: nothing in the crates or the port puts an envelope on a wire, so the port has no JSON of it to hold to the crate's"},
	{"fabric-controller", "src/controller.rs", "an_unhealthy_destination_is_refused", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "losing_the_destination_mid_flight_tears_the_action_down", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/controller.rs", "two_actions_cannot_run_on_one_target", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/decision.rs", "", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/execution.rs", "", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/fleet.rs", "", "fabric_controller_target.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/lib.rs", "", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/outcome.rs", "", "fabric_controller_plan.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/plan.rs", "", "fabric_controller_plan.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/policy.rs", "", "fabric_controller_target.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/replica.rs", "", "fabric_controller_core.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/target.rs", "", "fabric_controller_target.fct", "TestFabricControllerCore"},
	{"fabric-controller", "src/validation.rs", "", "fabric_controller_core.fct", "TestFabricControllerCore"},
	// fabric-core
	{"fabric-core", "src/atom.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/coordinate.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/engine.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/grid.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/lib.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/record.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/shard.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/storage.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/topology.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/value.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-core", "src/workload.rs", "", "fabric_core.fct", "TestFabricLeafDigestsMatchRust"},
	// fabric-daemon
	{"fabric-daemon", "src/admin.rs", "a_transfer_report_refuses_a_field_it_does_not_know", "fabric_daemon_admin.fct", "TestFabricDaemonAdminGoldenMatchesRust"},
	{"fabric-daemon", "src/admin.rs", "a_wrong_token_is_refused_however_close_it_is", "fabric_daemon_admin.fct", "TestFabricDaemonAdminGoldenMatchesRust"},
	{"fabric-daemon", "src/admin.rs", "an_absent_credential_never_matches_a_real_one", "fabric_daemon_admin.fct", "TestFabricDaemonAdminGoldenMatchesRust"},
	{"fabric-daemon", "src/config.rs", "a_keyspace_rule_for_an_unheld_cell_is_refused_at_startup", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "a_minimal_declaration_resolves_to_a_servable_fleet", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "a_missing_secret_names_the_variable_and_never_a_value", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "a_placement_store_without_a_credential_is_refused", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "a_silence_budget_shorter_than_a_probe_interval_is_refused", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "a_probe_timeout_not_shorter_than_the_silence_budget_is_refused", "fabric_daemon_config.fct", "TestFabricDaemonConfig"},
	{"fabric-daemon", "src/config.rs", "an_unknown_key_is_a_startup_error", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "debug_never_prints_a_secret", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "one_cell_may_not_be_declared_on_two_instances", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "policy_thresholds_come_from_the_file_and_default_conservatively", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "the_admin_surface_is_never_served_without_a_token", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/config.rs", "the_shipped_deployment_configuration_still_resolves", "fabric_daemon_config.fct", "TestFabricDaemonConfigRustTests"},
	{"fabric-daemon", "src/control.rs", "", "fabric_daemon_control.fct", "TestFabricDaemonControlGoldenMatchesRust"},
	{"fabric-daemon", "src/daemon.rs", "", "fabricd_lib.fct", "TestFabricDaemonProcess"},
	{"fabric-daemon", "src/lib.rs", "", "fabricd_lib.fct", "TestFabricDaemonTelemetry"},
	{"fabric-daemon", "src/liveness.rs", "nothing_listening_is_unreachable_rather_than_unhealthy", "fabric_daemon_liveness.fct", "TestFabricDaemonSweep"},
	{"fabric-daemon", "src/liveness.rs", "only_a_reachable_instance_files_a_heartbeat", "fabric_daemon_liveness.fct", "TestFabricDaemonProbe"},
	{"fabric-daemon", "src/main.rs", "", "fabricd.fct", "TestFabricDaemonMainMatchesRust"},
	{"fabric-daemon", "src/mover.rs", "a_refusal_names_the_action_it_is_about", "fabric_daemon_mover.fct", "TestFabricDaemonMoversGoldenMatchesRust"},
	{"fabric-daemon", "src/mover.rs", "concluded_actions_are_forgotten", "fabric_daemon_mover.fct", "TestFabricDaemonMoversGoldenMatchesRust"},
	{"fabric-daemon", "src/status.rs", "", "fabric_daemon_status.fct", "TestFabricDaemonStatusGoldenMatchesRust"},
	{"fabric-daemon", "src/telemetry.rs", "", "fabric_daemon_telemetry.fct", "TestFabricDaemonTelemetry"},
	{"fabric-daemon", "tests/daemon.rs", "an_operator_can_ask_for_a_move_and_is_held_to_the_controllers_checks", "fabricd_lib.fct", "TestFabricDaemonOperatorMigrate"},
	{"fabric-daemon", "tests/daemon.rs", "shutdown_rolls_back_an_action_that_has_not_cut_over", "fabric_daemon_control.fct", "TestFabricDaemonControlGoldenMatchesRust"},
	{"fabric-daemon", "tests/daemon.rs", "the_status_names_the_ports_actually_bound", "fabricd_lib.fct", "TestFabricDaemonProcess"},
	{"fabric-daemon", "tests/daemon.rs", "a_slow_stats_read_does_not_lose_nodes_that_answer_their_probes", "fabricd_lib.fct", "TestFabricDaemonSlowStatsKeepsNodesLive"},
	{"fabric-daemon", "tests/daemon.rs", "the_daemon_boots_routes_traffic_and_follows_a_control_loop_decision", "fabric_daemon_control.fct", "TestFabricDaemonControlGoldenMatchesRust"},
	{"fabric-daemon", "tests/mover.rs", "a_loaded_cell_crosses_the_pressure_threshold_from_real_facetql_stats", "fabric_facetql_poller.fct", "TestFabricDaemonLoadedCellCrossesThreshold"},
	{"fabric-daemon", "tests/mover.rs", "the_daemon_moves_a_cells_data_between_two_real_facetql_instances", "fabricd_lib.fct", "TestFabricDaemonMoverLive"},
	{"fabric-daemon", "tests/mover.rs", "writes_landing_after_the_snapshot_still_reach_the_destination", "fabricd_lib.fct", "TestFabricDaemonMoverLive"},
	// fabric-facetql
	{"fabric-facetql", "src/client.rs", "a_query_request_encodes_to_the_contract_body", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/client.rs", "debug_of_a_client_does_not_print_the_token", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/client.rs", "retry_after_is_read_as_delay_seconds_only", "fabric_facetql_client.fct", "TestFabricFacetqlRetryAfterMatchesRust"},
	{"fabric-facetql", "src/client.rs", "the_token_travels_in_the_header_and_never_in_the_url", "fabric_facetql_client.fct", "TestFabricFacetqlClientTraceAgainstRust"},
	{"fabric-facetql", "src/endpoint.rs", "a_missing_env_var_names_the_variable_not_a_value", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/endpoint.rs", "a_non_http_base_url_is_refused", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/endpoint.rs", "a_trailing_slash_does_not_double_up", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/endpoint.rs", "an_empty_token_is_refused_at_construction", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/endpoint.rs", "debug_never_prints_the_token", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/error.rs", "a_lost_race_does_not_condemn_the_instance", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/error.rs", "unreachable_and_unauthenticated_both_fail_closed", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/frontdoor.rs", "a_backend_url_must_be_http", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/frontdoor.rs", "a_credential_is_recognised_in_the_header_and_in_the_sse_fallback", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/frontdoor.rs", "a_front_door_with_no_backends_is_refused", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/frontdoor.rs", "a_request_spanning_two_backends_is_refused_naming_both_halves", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/frontdoor.rs", "every_routing_failure_has_a_status_a_client_can_act_on", "fabric_frontdoor.fct", "TestFrontDoorRoutingFailureStatuses"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "a_fallback_catches_what_no_rule_covers_and_shadows_nothing", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "a_half_declared_rule_is_refused", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "a_kind_and_its_addresses_resolve_to_the_same_key", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "a_namespace_wide_question_has_an_answer_only_when_everything_is_one_place", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "a_request_that_contradicts_itself_is_refused", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "an_ambiguous_keyspace_is_refused_at_construction", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "an_unmapped_request_is_refused_rather_than_guessed", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/keyspace.rs", "the_longest_declared_prefix_wins", "fabric.fct", "TestFabricLeafBKeyspace"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_body_that_is_not_the_contract_is_a_400_not_a_misroute", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_kind_scoped_aggregate_routes_by_its_kind", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_kind_scoped_query_routes_by_its_kind", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_multiget_names_every_address_it_asks_about", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_percent_encoded_address_routes_the_same_as_a_bare_one", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_point_read_routes_by_its_address", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_publish_routes_by_its_channel_to_exactly_one_backend", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_transaction_over_two_kinds_is_refused_not_split", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "a_write_is_planned_as_a_write", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "an_edge_names_both_of_its_ends", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "an_unknown_path_is_a_404_exactly_as_facetql_answers_it", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "an_unmapped_address_is_refused_rather_than_sent_somewhere_plausible", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "declarations_go_to_every_backend_and_listings_must_agree", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "every_transaction_op_shape_contributes_its_keys", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "namespace_wide_requests_are_refused_only_when_the_namespace_is_split", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/plan.rs", "the_missing_credential_message_is_facetqls_own_words", "fabric_plan.fct", "TestFabricLeafBPlan"},
	{"fabric-facetql", "src/frontdoor/proxy.rs", "a_frame_is_only_emitted_once_its_blank_line_has_arrived", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/frontdoor/proxy.rs", "a_stream_that_ends_ends_the_merge_rather_than_going_quiet", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "src/frontdoor/proxy.rs", "agreement_is_about_content_not_byte_order", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "src/frontdoor/proxy.rs", "hop_by_hop_headers_are_recognised_case_insensitively", "fabric_frontdoor.fct", "TestFrontDoorPureDecisions"},
	{"fabric-facetql", "src/lib.rs", "", "fabric_facetql_client.fct", "TestFabricFacetqlClientUnitsAgainstRust"},
	{"fabric-facetql", "src/mover.rs", "a_declared_cell_holds_its_own_kinds_and_nothing_else", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "a_fallback_cell_holds_everything_no_other_rule_claims", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "an_event_that_is_not_a_node_write_contributes_nothing", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "an_insert_op_carries_every_field_insert_node_has", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "every_write_shaped_event_yields_the_addresses_it_names", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "sse_frames_are_split_on_the_blank_line_in_either_encoding", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/mover.rs", "the_check_catches_the_fields_insert_node_cannot_carry", "fabric_facetql_mover.fct", "TestFabricFacetqlMoverUnitTests"},
	{"fabric-facetql", "src/placement.rs", "a_placement_round_trips_through_its_stored_form", "fabric_facetql_placement.fct", "TestFabricFacetqlPlacementUnitTests"},
	{"fabric-facetql", "src/placement.rs", "the_address_is_the_registry_key", "fabric_facetql_placement.fct", "TestFabricFacetqlPlacementUnitTests"},
	{"fabric-facetql", "src/placement.rs", "the_fabric_cell_lives_in_data_not_in_facetqls_coordinate", "fabric_facetql_placement.fct", "TestFabricFacetqlPlacementUnitTests"},
	{"fabric-facetql", "src/placement.rs", "the_set_map_of_an_update_carries_every_field_including_the_new_version", "fabric_facetql_placement.fct", "TestFabricFacetqlPlacementUnitTests"},
	{"fabric-facetql", "src/placement.rs", "undecodable_data_is_an_error_not_a_defaulted_placement", "fabric_facetql_placement.fct", "TestFabricFacetqlPlacementUnitTests"},
	{"fabric-facetql", "src/poller.rs", "a_credential_for_another_instance_is_refused", "fabric_facetql_poller.fct", "TestFabricFacetqlPollerUnitTests"},
	{"fabric-facetql", "src/poller.rs", "a_failed_poll_is_not_healthy", "fabric_facetql_poller.fct", "TestFabricFacetqlPollerUnitTests"},
	{"fabric-facetql", "src/poller.rs", "a_target_built_from_a_placement_inherits_its_cell", "fabric_facetql_poller.fct", "TestFabricFacetqlPollerUnitTests"},
	{"fabric-facetql", "src/poller.rs", "an_unreachable_instance_is_marked_unhealthy_not_left_alone", "fabric_facetql_poller.fct", "TestFabricFacetqlPollerUnitTests"},
	{"fabric-facetql", "src/poller.rs", "the_emitted_sample_round_trips_back_to_the_measured_metrics", "fabric_facetql_poller.fct", "TestFabricFacetqlPollerUnitTests"},
	{"fabric-facetql", "src/sample.rs", "a_quiet_engine_reports_low_queue_pressure", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "a_restart_drops_the_interval_instead_of_reporting_a_spike", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "a_saturated_engine_reports_maximum_queue_pressure", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "a_zero_length_interval_is_not_a_rate", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "an_idle_interval_is_zero_traffic_not_all_reads", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "an_unclosed_window_and_a_measured_idle_window_agree_on_absence", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "cpu_utilization_comes_from_the_monotonic_counter", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "nothing_facetql_does_not_measure_is_invented", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "overflow_marks_the_breakdown_as_partial", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "per_cell_traffic_is_differenced_into_rates", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/sample.rs", "two_samples_a_second_apart_are_a_rate", "fabric_stats.fct", "TestFabricLeafBSample"},
	{"fabric-facetql", "src/wire.rs", "a_current_stats_response_decodes_every_added_field", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "src/wire.rs", "a_query_request_omits_what_it_does_not_set", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "src/wire.rs", "delete_where_carries_its_predicate_under_the_key_where", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "src/wire.rs", "each_set_if_expectation_serializes_as_exactly_one_key", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "src/wire.rs", "stats_and_nodes_decode_from_facetqls_own_shape", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "src/wire.rs", "transaction_ops_match_the_canonical_contract", "fabric_facetql_wire.fct", "TestFabricFacetqlWireRustUnitTests"},
	{"fabric-facetql", "tests/front_door.rs", "a_facetql_client_cannot_tell_the_front_door_from_a_facetql", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_fence_that_outlasts_the_budget_becomes_a_retryable_503_and_reads_keep_working", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_multiget_spanning_two_backends_is_refused_rather_than_answered_in_part", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_request_without_a_credential_is_refused_in_facetqls_own_words", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_subscription_merges_every_instances_stream_and_never_goes_quietly_partial", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_subscription_one_instance_refuses_is_not_served_in_part", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_transaction_spanning_two_backends_is_refused_and_neither_engine_is_touched", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "a_write_fenced_by_a_cutover_waits_for_it_and_then_lands", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "declarations_reach_every_instance_and_a_divergent_listing_is_reported", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "in_front_of_a_single_facetql_the_door_is_fully_transparent", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "liveness_answers_for_the_whole_fleet", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "no_writable_replica_is_a_retryable_503_not_a_500", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/front_door.rs", "requests_with_no_true_fleet_answer_say_so_instead_of_faking_one", "fabric_frontdoor.fct", "TestFrontDoorScenariosMatchRust"},
	{"fabric-facetql", "tests/live.rs", "a_bad_token_fails_closed_rather_than_reading_as_healthy", "fabric_facetql_client.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "a_kind_larger_than_one_page_is_walked_by_cursor", "fabric_facetql_client.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "a_placement_is_written_and_read_back", "fabric_facetql_placement.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "a_stale_update_is_refused_by_the_engines_compare_and_set", "fabric_facetql_placement.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "claim_has_exactly_one_winner", "fabric_facetql_placement.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "polling_stats_drives_the_existing_optimizer_path", "fabric_facetql_poller.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	{"fabric-facetql", "tests/live.rs", "stats_reads_the_engines_own_counters", "fabric_facetql_client.fct", "TestFabricFacetqlLiveAgainstRealFacetql"},
	// fabric-migration
	{"fabric-migration", "src/lib.rs", "", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "a_failure_during_cleanup_leaves_the_destination_owning_the_shard", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "a_plan_to_the_same_node_is_refused", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "a_replayed_write_to_the_source_is_refused", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "a_write_is_never_applied_twice_across_the_handover", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "abort_is_available_up_to_cutover_and_never_after", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "catch_up_cannot_begin_with_atoms_missing", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "cutover_cannot_complete_while_a_write_is_undrained", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "reads_are_servable_in_every_phase", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "status_reports_progress_without_advancing_anything", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "the_destination_cannot_claim_a_write_the_source_never_took", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/migration.rs", "writes_are_refused_while_the_fence_is_up", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/phase.rs", "a_terminal_phase_goes_nowhere", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/phase.rs", "abort_stops_being_available_once_authority_has_moved", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/phase.rs", "cutover_cannot_be_skipped", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/plan.rs", "", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	{"fabric-migration", "src/progress.rs", "", "fabric_migration_plan.fct", "TestFabricLeafBMigration"},
	// fabric-ml
	{"fabric-ml", "src/features.rs", "", "fabric_ml.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-ml", "src/lib.rs", "", "fabric_ml.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-ml", "src/predictor.rs", "is_likely_hot_is_a_pure_function_of_its_own_two_fields", "fabric_ml.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-ml", "src/predictor.rs", "is_likely_hot_respects_the_predictors_own_configured_threshold", "fabric_ml.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-ml", "src/predictor.rs", "the_default_threshold_matches_pressure_level_highs_cutoff", "fabric_ml.fct", "TestFabricLeafDigestsMatchRust"},
	// fabric-optimizer
	{"fabric-optimizer", "src/decision.rs", "", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/fleet.rs", "a_destination_is_always_a_node_the_fleet_named", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/fleet.rs", "a_fleet_derived_from_the_map_names_only_nodes_that_hold_data", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/fleet.rs", "a_node_that_cannot_take_the_copy_is_not_a_candidate", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/fleet.rs", "a_replica_is_ranked_out_of_the_source_region_first", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/fleet.rs", "the_topology_a_fleet_exposes_locates_by_shard_not_by_guessing", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/lib.rs", "", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_bare_topology_still_names_a_node_that_holds_data", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_coordinate_two_shards_share_is_resolved_by_its_own_shard_id", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_fleet_with_nowhere_to_go_produces_no_action", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_mixed_hotspot_moves_to_the_least_loaded_node", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_named_destination_is_always_a_real_node", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_read_heavy_hotspot_replicates_out_of_its_own_region", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-optimizer", "src/optimizer.rs", "a_write_heavy_hotspot_still_isolates_without_naming_a_node", "fabric_optimizer.fct", "TestFabricLeafDigestsMatchRust"},
	// fabric-protocol
	{"fabric-protocol", "src/bin/facet-protocol.rs", "", "fabric_protocol_server.fct", "TestFabricProtocolServerGoldenMatchesRealBinary"},
	{"fabric-protocol", "src/lib.rs", "", "fabric_protocol.fct", "TestFabricProtocolDecodeMatchesRust"},
	{"fabric-protocol", "src/message.rs", "", "fabric_protocol.fct", "TestFabricProtocolDecodeMatchesRust"},
	{"fabric-protocol", "src/node.rs", "", "fabric_protocol.fct", "TestFabricProtocolDecodeMatchesRust"},
	{"fabric-protocol", "src/observation.rs", "", "fabric_protocol.fct", "TestFabricProtocolDecodeMatchesRust"},
	{"fabric-protocol", "src/topology.rs", "", "fabric_protocol.fct", "TestFabricProtocolDecodeMatchesRust"},
	{"fabric-protocol", "src/transport.rs", "a_wrong_token_is_refused_however_close_it_is", "fabric_protocol.fct", "TestFabricProtocolServer"},
	{"fabric-protocol", "src/transport.rs", "an_absent_credential_never_matches_a_real_one", "fabric_protocol.fct", "TestFabricProtocolServer"},
	{"fabric-protocol", "tests/auth.rs", "a_malformed_body_with_no_token_is_still_refused_as_unauthorized_first", "fabric_protocol_server.fct", "TestFabricProtocolServer"},
	{"fabric-protocol", "tests/auth.rs", "a_request_with_no_token_is_refused", "fabric_protocol_server.fct", "TestFabricProtocolServer"},
	{"fabric-protocol", "tests/auth.rs", "a_request_with_the_right_token_is_handled", "fabric_protocol_server.fct", "TestFabricProtocolServer"},
	{"fabric-protocol", "tests/auth.rs", "a_request_with_the_wrong_token_is_refused", "fabric_protocol_server.fct", "TestFabricProtocolServer"},
	// fabric-replication
	{"fabric-replication", "src/failover.rs", "a_failed_primary_names_the_promotion_but_does_not_perform_it", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/failover.rs", "a_lagging_candidate_is_refused_with_its_reason", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/failover.rs", "a_serving_primary_produces_no_promotion", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/failover.rs", "a_silent_primary_needs_replacement_even_while_marked_in_sync", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/lib.rs", "", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/placement.rs", "a_plan_never_puts_two_copies_on_one_node", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/placement.rs", "a_strict_region_rule_reports_a_shortfall_instead_of_doubling_up", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/placement.rs", "an_unserviceable_node_is_never_planned_onto", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/replica.rs", "a_repeated_state_is_idempotent_but_absent_is_not", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/replica.rs", "the_lifecycle_graph_has_no_shortcut_into_service", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "a_copy_that_is_not_in_sync_cannot_be_promoted", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "a_new_copy_must_seed_before_it_can_serve", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "a_report_for_another_shard_is_refused", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "an_out_of_order_report_is_ignored", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "lag_reports_move_a_secondary_between_in_sync_and_lagging", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "promotion_is_a_swap_so_there_is_always_exactly_one_primary", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "the_primary_cannot_be_removed_out_from_under_the_shard", "fabric_replication.fct", "TestFabricLeafBReplication"},
	{"fabric-replication", "src/set.rs", "unreachability_only_fails_a_copy_after_the_grace_period", "fabric_replication.fct", "TestFabricLeafBReplication"},
	// fabric-routing
	{"fabric-routing", "src/error.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/key.rs", "a_malformed_address_says_what_was_expected", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/key.rs", "an_address_round_trips", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/key.rs", "an_off_grid_coordinate_is_refused", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/lib.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/range.rs", "a_backwards_range_is_refused", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/range.rs", "a_full_range_covers_every_atom_of_the_grid", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/range.rs", "indexing_round_trips_through_the_grid", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/route.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-routing", "src/table.rs", "a_cell_scoped_migration_is_invisible_to_its_siblings", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_cell_scoped_publication_needs_a_shard_to_scope_it_to", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_cell_scoped_replica_set_answers_for_that_cell_alone", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_failed_primary_leaves_reads_working_and_writes_unroutable", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_range_answers_per_cell_and_fails_whole_on_a_fenced_one", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_range_resolves_into_one_segment_per_node", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_scan_fails_rather_than_returning_a_partial_answer", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "a_shard_wide_publication_still_answers_for_every_cell", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "an_aborted_migration_leaves_the_source_owning_the_shard", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "an_unknown_shard_is_named_as_such", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "an_unreachable_owner_is_an_explicit_no_live_owner", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "every_scoped_mutation_moves_the_generation", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "placement_comes_from_topology_not_from_the_identity", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "reads_and_writes_diverge_once_there_are_replicas", "fabric_table.fct", "TestFabricLeafBTable"},
	{"fabric-routing", "src/table.rs", "routes_stay_correct_for_the_whole_migration", "fabric_table.fct", "TestFabricLeafBTable"},
	// fabric-runtime
	{"fabric-runtime", "src/lib.rs", "a_clock_tick_alone_takes_a_silent_node_out_of_routing", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "a_heartbeat_from_an_unregistered_node_is_rejected", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "a_heartbeat_updates_the_registry_and_the_clock", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "a_node_that_stops_beating_goes_unreachable", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "an_operator_policy_reaches_the_controller", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "registration_is_retained_not_just_acknowledged", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "telemetry_also_advances_the_clock", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/lib.rs", "the_observation_window_is_bounded", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "a_cutover_waits_for_the_copy_to_be_checked_and_fails_if_it_does_not", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "a_replicate_adds_a_holder_and_removes_none", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "a_rollback_after_cutover_says_it_is_irreversible", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "a_rollback_before_cutover_puts_everything_back", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "a_split_is_declined_rather_than_faked", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "isolate_picks_its_own_destination_through_the_replication_planner", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "one_cell_of_a_shard_moves_without_disturbing_its_siblings", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/mechanism.rs", "routing_learns_about_every_phase_the_controller_advances", "fabric_runtime_mechanism.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/placement.rs", "", "fabric_runtime_placement.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "a_freshly_registered_node_is_not_yet_alive", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "a_heartbeat_from_an_unregistered_node_is_refused", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "a_self_reported_failure_is_degraded_not_healthy", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "an_out_of_order_heartbeat_does_not_rewind_liveness", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "heartbeat_makes_a_node_healthy_until_the_deadline", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "re_registration_resets_proof_of_life", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "registration_is_retained_and_listed", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/registry.rs", "unhealthy_lists_everything_not_serviceable", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "src/state.rs", "", "fabric_runtime_registry.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "tests/control_loop.rs", "a_real_optimizer_move_runs_one_cell_of_a_shard_to_a_measured_verdict", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "tests/control_loop.rs", "a_real_optimizer_replicate_reaches_a_measured_verdict", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	{"fabric-runtime", "tests/control_loop.rs", "the_controller_executes_a_real_decision_through_the_real_mechanisms", "fabric_runtime.fct", "TestFabricRuntimeGoldenMatchesRust"},
	// fabric-simulator
	{"fabric-simulator", "src/clock.rs", "", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	{"fabric-simulator", "src/cluster.rs", "", "fabric_simulator_cluster.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/executor.rs", "", "fabric_simulator_cluster.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/fault.rs", "", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	{"fabric-simulator", "src/lib.rs", "", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/rng.rs", "different_seeds_diverge_immediately", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	{"fabric-simulator", "src/rng.rs", "floats_stay_in_the_unit_interval", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	{"fabric-simulator", "src/rng.rs", "the_same_seed_gives_the_same_sequence", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	{"fabric-simulator", "src/simulation.rs", "a_different_seed_produces_a_different_run", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "a_hot_shard_shows_up_as_pressure_on_its_cell", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "a_lost_node_goes_silent_rather_than_reporting_zeroes", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "a_partitioned_region_keeps_serving_but_stops_reporting", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "a_split_is_declined_rather_than_faked", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "the_controller_executes_and_measures_against_the_simulation", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "the_optimizer_runs_against_the_simulator_unchanged", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/simulation.rs", "the_same_seed_reproduces_the_run_exactly", "fabric_simulator.fct", "TestFabricSimulator"},
	{"fabric-simulator", "src/workload.rs", "", "fabric_simulator_base.fct", "TestFabricSimulatorBase"},
	// fabric-telemetry
	{"fabric-telemetry", "src/lib.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-telemetry", "src/metrics.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-telemetry", "src/observation.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
	// fabric-topology
	{"fabric-topology", "src/lib.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-topology", "src/placement.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-topology", "src/registry.rs", "", "fabric_routing.fct", "TestFabricLeafDigestsMatchRust"},
	// fabric-workload
	{"fabric-workload", "src/analyzer.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-workload", "src/lib.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
	{"fabric-workload", "src/profile.rs", "", "fabric_telemetry.fct", "TestFabricLeafDigestsMatchRust"},
}

const fabricCratesDir = "../../fabric/crates"

var (
	// A #[test] or #[tokio::test(...)] attribute, any further attributes,
	// then the fn: what fabric_check's authors and cargo both count.
	fabricRustTestRe = regexp.MustCompile(`#\[(?:tokio::)?test[^\]]*\]\s*(?:#\[[^\]]*\]\s*)*(?:pub\s+)?(?:async\s+)?fn\s+(\w+)`)
	fabricGoTestRe   = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
)

// fabricRustTests walks fabric/crates and answers every test as
// "crate\tfile\tname", plus every src module as "crate\tfile\t".
func fabricRustTests(t *testing.T) (tests, modules map[string]bool) {
	t.Helper()
	tests, modules = map[string]bool{}, map[string]bool{}
	crates, err := os.ReadDir(fabricCratesDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range crates {
		if !c.IsDir() {
			continue
		}
		base := filepath.Join(fabricCratesDir, c.Name())
		err := filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".rs") {
				return nil
			}
			rel, _ := filepath.Rel(base, path)
			rel = filepath.ToSlash(rel)
			if !strings.HasPrefix(rel, "src/") && !strings.HasPrefix(rel, "tests/") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.HasPrefix(rel, "src/") {
				modules[c.Name()+"\t"+rel+"\t"] = true
			}
			for _, m := range fabricRustTestRe.FindAllStringSubmatch(string(src), -1) {
				tests[c.Name()+"\t"+rel+"\t"+m[1]] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return tests, modules
}

// fabricGoTests answers every Test function declared in this package.
func fabricGoTests(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range fabricGoTestRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

func TestFabricCoverage(t *testing.T) {
	if _, err := os.Stat(fabricCratesDir); err != nil {
		t.Fatalf("the Rust reference is not beside this tree: %v", err)
	}
	rustTests, rustModules := fabricRustTests(t)
	goTests := fabricGoTests(t)

	rowed := map[string]bool{}
	for _, r := range fabricCoverage {
		key := r.crate + "\t" + r.file + "\t" + r.test
		if rowed[key] {
			t.Errorf("%s %s %s: listed twice", r.crate, r.file, r.test)
		}
		rowed[key] = true
		name := r.crate + "/" + r.file
		if r.test != "" {
			name += "/" + r.test
		}
		t.Run(name, func(t *testing.T) {
			if r.test != "" && !rustTests[key] {
				t.Fatalf("no #[test] fn %s in fabric/crates/%s/%s", r.test, r.crate, r.file)
			}
			if r.test == "" && !rustModules[key] {
				t.Fatalf("no module fabric/crates/%s/%s", r.crate, r.file)
			}
			if r.port == "" {
				t.Skip("not ported: " + r.goTest)
			}
			if _, err := os.Stat(r.port); err != nil {
				t.Fatalf("port %s: %v", r.port, err)
			}
			if !goTests[r.goTest] {
				t.Fatalf("no Go test %s in selfhost", r.goTest)
			}
		})
	}
	// The other direction: nothing in the crates is off the table.
	var missing []string
	for k := range rustTests {
		if !rowed[k] {
			missing = append(missing, strings.ReplaceAll(k, "\t", " "))
		}
	}
	for k := range rustModules {
		if rowed[k] {
			continue
		}
		// A module with tests is on the table through them.
		covered := false
		for rk := range rowed {
			if strings.HasPrefix(rk, k) {
				covered = true
				break
			}
		}
		if !covered {
			missing = append(missing, strings.ReplaceAll(k, "\t", " ")+"(module)")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("fabric/crates has %s and the table has no row for it", m)
	}
}
